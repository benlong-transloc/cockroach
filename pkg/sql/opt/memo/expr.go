// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package memo

import (
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
)

// ExprOrdinal is the ordinal position of an expression within its memo group.
// Each group stores one or more logically equivalent expressions. The 0th
// expression is always the normalized expression for the group.
type ExprOrdinal uint16

const (
	// normExprOrdinal is the ordinal position of the normalized expression in
	// its group.
	normExprOrdinal ExprOrdinal = 0
)

// ExprID uniquely identifies an expression stored in the memo by pairing the
// ID of its group with the ordinal position of the expression within that
// group.
type ExprID struct {
	Group GroupID
	Expr  ExprOrdinal
}

// InvalidExprID is the uninitialized ExprID that never points to a valid
// expression.
var InvalidExprID = ExprID{}

// Suppress linter complaint.
var _ = InvalidExprID

// MakeNormExprID returns the id of the normalized expression for the given
// group.
func MakeNormExprID(group GroupID) ExprID {
	return ExprID{Group: group, Expr: normExprOrdinal}
}

// ReplaceChildFunc is the callback function passed to the Expr.Replace method.
// It is called with each child group of the expression. See the Replace method
// for more details.
type ReplaceChildFunc func(child GroupID) GroupID

// exprState is opaque storage used to store operator-specific fields in the
// memo expression.
type exprState [opt.MaxOperands]uint32

// Expr is a memoized representation of an expression. Strongly-typed
// specializations of Expr are generated by optgen for each operator (see
// expr.og.go). Each Expr belongs to a memo group, which contains logically
// equivalent expressions. Two expressions are considered logically equivalent
// if they both reduce to an identical normal form after normalizing
// transformations have been applied.
//
// The children of Expr are recursively memoized in the same way as the
// Expr, and are referenced by their memo group. Therefore, the Expr
// is the root of a forest of expressions.
type Expr struct {
	// op is this expression's operator type. Each operator may have additional
	// fields. To access these fields in a strongly-typed way, use the asXXX()
	// generated methods to cast the Expr to the more specialized
	// expression type.
	op opt.Operator

	// state stores operator-specific state. Depending upon the value of the
	// op field, this state will be interpreted in different ways.
	state exprState
}

// Operator returns this memo expression's operator type. Each operator may
// have additional fields. To access these fields in a strongly-typed way, use
// the AsXXX() generated methods to cast the Expr to the more specialized
// expression type.
func (e *Expr) Operator() opt.Operator {
	return e.op
}

// Fingerprint uniquely identifies a memo expression by combining its operator
// type plus its operator fields. It can be used as a map key. If two
// expressions share the same fingerprint, then they are the identical
// expression. If they don't share a fingerprint, then they still may be
// logically equivalent expressions. Since a memo expression is 16 bytes and
// contains no pointers, it can function as its own fingerprint/hash.
type Fingerprint Expr

// Fingerprint returns this memo expression's unique fingerprint.
func (e *Expr) Fingerprint() Fingerprint {
	return Fingerprint(*e)
}

// opLayout describes the "layout" of each op's children. It contains multiple
// fields:
//
//  - fixedCount (bits 0,1):
//      number of children, excluding any list; the children are in
//      state[0] through state[fixedCount-1].
//
//  - list (bits 2,3):
//      0 if op has no list, otherwise 1 + position of the list in state
//      (specifically Offset=state[list-1] Length=state[list]).
//
//  - priv (bits 4,5):
//      0 if op has no private, otherwise 1 + position of the private in state.
//
// The table of values (opLayoutTable) is generated by optgen.
type opLayout uint8

func (val opLayout) fixedCount() uint8 {
	return uint8(val) & 3
}

func (val opLayout) list() uint8 {
	return (uint8(val) >> 2) & 3
}

func (val opLayout) priv() uint8 {
	return (uint8(val) >> 4) & 3
}

func makeOpLayout(fixedCount, list, priv uint8) opLayout {
	return opLayout(fixedCount | (list << 2) | (priv << 4))
}

// ChildCount returns the number of expressions that are inputs to this parent
// expression.
func (e *Expr) ChildCount() int {
	layout := opLayoutTable[e.op]
	fixedCount := layout.fixedCount()
	list := layout.list()
	if list == 0 {
		return int(fixedCount)
	}
	return int(fixedCount) + int(e.state[list])
}

// ChildGroup returns the memo group containing the nth child of this parent
// expression.
func (e *Expr) ChildGroup(mem *Memo, nth int) GroupID {
	layout := opLayoutTable[e.op]
	fixedCount := layout.fixedCount()
	if nth < int(fixedCount) {
		return GroupID(e.state[nth])
	}
	nth -= int(fixedCount)
	list := layout.list()
	if list != 0 && nth < int(e.state[list]) {
		listID := ListID{Offset: e.state[list-1], Length: e.state[list]}
		return mem.LookupList(listID)[nth]
	}
	panic("child index out of range")
}

// Replace invokes the given callback function for each child memo group of the
// expression, including both fixed and list children. The callback function can
// return the unchanged group, or it can construct a new group and return that
// instead. Replace will assemble all of the changed and unchanged children and
// return a new expression of the same type having those children. Callers can
// use this method as a building block when searching and replacing expressions
// in a tree.
func (e *Expr) Replace(mem *Memo, replace ReplaceChildFunc) Expr {
	return MakeExpr(e.op, e.ReplaceOperands(mem, replace))
}

// ReplaceOperands invokes the given callback function for each child memo
// group of the expression, including both fixed and list children. The
// callback function can return the unchanged group, or it can construct a new
// group and return that instead. Replace will assemble all of the changed and
// unchanged children and return them as DynamicOperands, which can be passed
// to MakeExpr or DynamicConstruct to create the new expression. Callers can
// use this method as a building block when searching and replacing expressions
// in a tree.
func (e *Expr) ReplaceOperands(mem *Memo, replace ReplaceChildFunc) DynamicOperands {
	var operands DynamicOperands
	layout := opLayoutTable[e.op]

	// Visit each fixed child.
	nth := int(layout.fixedCount())
	for i := 0; i < nth; i++ {
		operands[i] = DynamicID(replace(GroupID(e.state[i])))
	}

	// Visit each list child.
	list := layout.list()
	if list != 0 {
		listID := ListID{Offset: e.state[list-1], Length: e.state[list]}
		oldList := mem.LookupList(listID)
		newList := make([]GroupID, len(oldList))
		for i := 0; i < len(oldList); i++ {
			newList[i] = replace(oldList[i])
		}
		operands[nth] = MakeDynamicListID(mem.InternList(newList))
		nth++
	}

	// Append the private and return the operands.
	privateID := e.PrivateID()
	if privateID != 0 {
		operands[nth] = DynamicID(privateID)
	}
	return operands
}

// Private returns the value of this expression's private field, if it has one,
// or nil if not.
func (e *Expr) Private(mem *Memo) interface{} {
	return mem.LookupPrivate(e.PrivateID())
}

// PrivateID returns the interning identifier of this expression's private
// field, or 0 if no private field exists.
func (e *Expr) PrivateID() PrivateID {
	priv := opLayoutTable[e.op].priv()
	if priv == 0 {
		return 0
	}
	return PrivateID(e.state[priv-1])
}
