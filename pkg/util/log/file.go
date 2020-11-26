// Copyright 2013 Google Inc. All Rights Reserved.
// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.
//
// This code originated in the github.com/golang/glog package.

package log

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/cli/exit"
	"github.com/cockroachdb/cockroach/pkg/util/log/logpb"
	"github.com/cockroachdb/cockroach/pkg/util/log/severity"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/errors/oserror"
)

// File I/O for logs.

// TemporarilyDisableFileGCForMainLogger disables the file-based GC until the
// cleanup fn is called. Note that the behavior is undefined if this
// is called from multiple concurrent goroutines.
//
// For background about why this function exists, see:
// https://github.com/cockroachdb/cockroach/issues/36861#issuecomment-483589446
func TemporarilyDisableFileGCForMainLogger() (cleanup func()) {
	fileSink := debugLog.getFileSink()
	if fileSink == nil || !fileSink.enabled.Get() {
		return func() {}
	}
	oldLogLimit := atomic.LoadInt64(&fileSink.logFilesCombinedMaxSize)
	atomic.CompareAndSwapInt64(&fileSink.logFilesCombinedMaxSize, oldLogLimit, math.MaxInt64)
	return func() {
		atomic.CompareAndSwapInt64(&fileSink.logFilesCombinedMaxSize, math.MaxInt64, oldLogLimit)
	}
}

// fileSink represents a file sink.
type fileSink struct {
	// whether the sink is enabled.
	// This should only be written while mu is held, but can
	// be read anytime. We maintain the invariant that
	// enabled = true implies logDir != "".
	enabled syncutil.AtomicBool

	// name prefix for log files.
	prefix string

	// syncWrites if true calls file.Flush and file.Sync on every log
	// write. This can be set per-logger e.g. for audit logging.
	//
	// Note that synchronization for all log files simultaneously can
	// also be configured via logging.syncWrites, see SetSync().
	syncWrites bool

	// logFileMaxSize is the maximum size of a log file in bytes.
	logFileMaxSize int64

	// logFilesCombinedMaxSize is the maximum total size in bytes for log
	// files generated by one logger. Note that this is only checked when
	// log files are created, so the total size of log files might
	// temporarily be up to logFileMaxSize larger.
	logFilesCombinedMaxSize int64

	// Level beyond which entries submitted to this sink are written
	// to the output file. This acts as a filter between the log entry
	// producers and the file sink.
	threshold Severity

	// formatter for entries.
	formatter logFormatter

	// notify GC daemon that a new log file was created.
	gcNotify chan struct{}

	// getStartLines retrieves a list of log entries to
	// include at the start of a log file.
	getStartLines func(time.Time) []logpb.Entry

	// mu protects the remaining elements of this structure and is
	// used to synchronize output to this file sink..
	mu struct {
		syncutil.Mutex

		// directory prefix where to store this logger's files. This is
		// under "mu" because the test Scope can overwrite this
		// asynchronously.
		logDir string

		// file holds the log file writer.
		file flushSyncWriter

		// redirectInternalStderrWrites, when set, causes this file sink to
		// capture writes to system-wide file descriptor 2 (the standard
		// error stream) and os.Stderr and redirect them to this sink's
		// output file.
		// This is managed by the takeOverInternalStderr() method.
		//
		// Note that this mechanism redirects file descriptor 2, and does
		// not only assign a different *os.File reference to
		// os.Stderr. This is because the Go runtime hardcodes stderr writes
		// as writes to file descriptor 2 and disregards the value of
		// os.Stderr entirely.
		//
		// There can be at most one file sink with this boolean set. This
		// constraint is enforced by takeOverInternalStderr().
		redirectInternalStderrWrites bool

		// currentlyOwnsInternalStderr determines whether this file sink
		// _currently_ has taken over fd 2. This may be false while
		// redirectInternalStderrWrites above is true, when the sink has
		// not yet opened its output file, or is in the process of
		// switching over from one directory to the next.
		currentlyOwnsInternalStderr bool
	}
}

// newFileSink creates a new file sink.
func newFileSink(
	dir, fileNamePrefix string,
	forceSyncWrites bool,
	fileThreshold Severity,
	fileMaxSize, combinedMaxSize int64,
	getStartLines func(time.Time) []logpb.Entry,
) *fileSink {
	prefix := program
	if fileNamePrefix != "" {
		prefix = program + "-" + fileNamePrefix
	}
	f := &fileSink{
		prefix:                  prefix,
		threshold:               fileThreshold,
		formatter:               formatCrdbV1WithCounter{},
		syncWrites:              forceSyncWrites,
		logFileMaxSize:          fileMaxSize,
		logFilesCombinedMaxSize: combinedMaxSize,
		gcNotify:                make(chan struct{}, 1),
		getStartLines:           getStartLines,
	}
	f.mu.logDir = dir
	f.enabled.Set(dir != "")
	return f
}

// activeAtSeverity implements the logSink interface.
func (l *fileSink) activeAtSeverity(sev logpb.Severity) bool {
	return l.enabled.Get() && sev >= l.threshold
}

// attachHints implements the logSink interface.
func (l *fileSink) attachHints(stacks []byte) []byte {
	// The Fatal output will be copied across multiple sinks, so it may
	// show up to a (human) observer through a different channel than a
	// file in the log directory. So remind the operator where to look
	// for more details.
	l.mu.Lock()
	stacks = append(stacks, []byte(fmt.Sprintf(
		"\nFor more context, check log files in: %s\n", l.mu.logDir))...)
	l.mu.Unlock()
	return stacks
}

// getFormatter implements the logSink interface.
func (l *fileSink) getFormatter() logFormatter {
	return l.formatter
}

// output implements the logSink interface.
func (l *fileSink) output(extraSync bool, b []byte) error {
	if !l.enabled.Get() {
		// NB: we need to check filesink.enabled a second time here in
		// case a test Scope() has disabled it asynchronously while
		// (*loggerT).outputLogEntry() was not holding outputMu.
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.ensureFileLocked(); err != nil {
		return err
	}

	if err := l.writeToFileLocked(b); err != nil {
		return err
	}

	if extraSync || l.syncWrites || logging.syncWrites.Get() {
		l.flushAndSyncLocked(true /*doSync*/)
	}
	return nil
}

// exitCode implements the logSink interface.
func (l *fileSink) exitCode() exit.Code {
	return exit.LoggingFileUnavailable()
}

// emergencyOutput implements the logSink interface.
func (l *fileSink) emergencyOutput(b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.ensureFileLocked(); err != nil {
		return //nolint:returnerrcheck
	}

	if err := l.writeToFileLocked(b); err != nil {
		return //nolint:returnerrcheck
	}

	// During an emergency, we flush to get the data out to the OS, but
	// we don't care as much about persistence. In fact, trying too hard
	// to sync may cause additional stoppage.
	l.flushAndSyncLocked(false /*doSync*/)
}

// lockAndFlushAndSync is like flushAndSync but locks l.mu first.
func (l *fileSink) lockAndFlushAndSync(doSync bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushAndSyncLocked(doSync)
}

// flushAndSync flushes the current log and, if doSync is set,
// attempts to sync its data to disk.
//
// l.mu is held.
func (l *fileSink) flushAndSyncLocked(doSync bool) {
	if l.mu.file == nil {
		return
	}

	// TODO(knz): the following stall detection code is misplaced.
	// See: https://github.com/cockroachdb/cockroach/issues/56893
	//
	// If we can't flush or sync within this duration, exit the process.
	t := time.AfterFunc(maxSyncDuration, func() {
		// NB: the disk-stall-detected roachtest matches on this message.
		Shoutf(context.Background(), severity.FATAL,
			"disk stall detected: unable to sync log files within %s", maxSyncDuration,
		)
	})
	defer t.Stop()
	// If we can't flush sync within this duration, print a warning to the log and to
	// stderr.
	t2 := time.AfterFunc(syncWarnDuration, func() {
		Shoutf(context.Background(), severity.WARNING,
			"disk slowness detected: unable to sync log files within %s", syncWarnDuration,
		)
	})
	defer t2.Stop()

	_ = l.mu.file.Flush() // ignore error
	if doSync {
		_ = l.mu.file.Sync() // ignore error
	}
}

// DirName overrides (if non-empty) the choice of directory in
// which to write logs.
type DirName string

var _ flag.Value = (*DirName)(nil)

// Set implements the flag.Value interface.
func (l *DirName) Set(dir string) error {
	if len(dir) > 0 && dir[0] == '~' {
		return fmt.Errorf("log directory cannot start with '~': %s", dir)
	}
	if len(dir) > 0 {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		dir = absDir
	}
	*l = DirName(dir)
	return nil
}

// Type implements the flag.Value interface.
func (l *DirName) Type() string {
	return "string"
}

// String implements the flag.Value interface.
func (l *DirName) String() string {
	return string(*l)
}

func (l *DirName) get() (dirName string, isSet bool) {
	return string(*l), *l != ""
}

// IsSet returns true iff the directory name is set.
func (l *DirName) IsSet() bool {
	res := *l != ""
	return res
}

// DirSet returns true iff the log directory has been changed from the
// command line.
func DirSet() bool { return logging.logDir.IsSet() }

var (
	pid      = os.Getpid()
	program  = filepath.Base(os.Args[0])
	host     = "unknownhost"
	userName = "unknownuser"
)

func init() {
	h, err := os.Hostname()
	if err == nil {
		host = shortHostname(h)
	}

	current, err := user.Current()
	if err == nil {
		userName = current.Username
	}

	// Sanitize userName since it may contain filepath separators on Windows.
	userName = strings.Replace(userName, `\`, "_", -1)
}

// shortHostname returns its argument, truncating at the first period.
// For instance, given "www.google.com" it returns "www".
func shortHostname(hostname string) string {
	if i := strings.IndexByte(hostname, '.'); i >= 0 {
		return hostname[:i]
	}
	return hostname
}

// FileTimeFormat is RFC3339 with the colons replaced with underscores.
// It is the format used for timestamps in log file names.
// This removal of colons creates log files safe for Windows file systems.
const FileTimeFormat = "2006-01-02T15_04_05Z07:00"

// removePeriods removes all extraneous periods. This is required to ensure that
// the only periods in the filename are the ones added by logName so it can
// be easily parsed.
func removePeriods(s string) string {
	return strings.Replace(s, ".", "", -1)
}

// logName returns a new log file name with start time t, and the name
// for the symlink.
func logName(prefix string, t time.Time) (name, link string) {
	name = fmt.Sprintf("%s.%s.%s.%s.%06d.log",
		removePeriods(prefix),
		removePeriods(host),
		removePeriods(userName),
		t.Format(FileTimeFormat),
		pid)
	return name, removePeriods(prefix) + ".log"
}

var errDirectoryNotSet = errors.New("log: log directory not set")

// create creates a new log file and returns the file and its
// filename. If the file is created successfully, create also attempts
// to update the symlink for that tag, ignoring errors.
//
// It is invalid to call this with an unset output directory.
func create(
	dir, prefix string, t time.Time, lastRotation int64,
) (f *os.File, updatedRotation int64, filename, symlink string, err error) {
	if dir == "" {
		return nil, lastRotation, "", "", errDirectoryNotSet
	}

	// Ensure that the timestamp of the new file name is greater than
	// the timestamp of the previous generated file name.
	unix := t.Unix()
	if unix <= lastRotation {
		unix = lastRotation + 1
	}
	updatedRotation = unix
	t = timeutil.Unix(unix, 0)

	// Generate the file name.
	name, link := logName(prefix, t)
	symlink = filepath.Join(dir, link)
	fname := filepath.Join(dir, name)
	// Open the file os.O_APPEND|os.O_CREATE rather than use os.Create.
	// Append is almost always more efficient than O_RDRW on most modern file systems.
	f, err = os.OpenFile(fname, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	return f, updatedRotation, fname, symlink, errors.Wrapf(err, "log: cannot create output file")
}

func createSymlink(fname, symlink string) {
	// Symlinks are best-effort.
	if err := os.Remove(symlink); err != nil && !oserror.IsNotExist(err) {
		fmt.Fprintf(OrigStderr, "log: failed to remove symlink %s: %s\n", symlink, err)
	}
	if err := os.Symlink(filepath.Base(fname), symlink); err != nil {
		// On Windows, this will be the common case, as symlink creation
		// requires special privileges.
		// See: https://docs.microsoft.com/en-us/windows/device-security/security-policy-settings/create-symbolic-links
		if runtime.GOOS != "windows" {
			fmt.Fprintf(OrigStderr, "log: failed to create symlink %s: %s\n", symlink, err)
		}
	}
}
