// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package execinfra

// OpNode is an interface to operator-like structures with children.
type OpNode interface {
	// TODO(yuzefovich): modify the interface so that a boolean is passed in to
	// distinguish between verbose and non-verbose outputs.
	// ChildCount returns the number of children (inputs) of the operator.
	ChildCount() int

	// Child returns the nth child (input) of the operator.
	Child(nth int) OpNode
}
