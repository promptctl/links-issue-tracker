package main

import "strings"

// touch is one thing outside the test's process that a test can reach.
type touch int

const (
	cli touch = iota
	store
	git
	process
	fs
	clockWait
	clockRead
	touchCount
)

// touchNames are the touches as the table and listing print them, in column
// order.
var touchNames = [touchCount]string{
	cli:       "cli",
	store:     "store",
	git:       "git",
	process:   "proc",
	fs:        "fs",
	clockWait: "clock-wait",
	clockRead: "clock-read",
}

func (t touch) String() string { return touchNames[t] }

// touchSet is the set of touches one test reaches.
type touchSet uint

func (s touchSet) has(t touch) bool      { return s&(1<<t) != 0 }
func (s touchSet) with(t touch) touchSet { return s | 1<<t }
func (s touchSet) any(ts ...touch) bool {
	for _, t := range ts {
		if s.has(t) {
			return true
		}
	}
	return false
}

func (s touchSet) String() string {
	var names []string
	for t := touch(0); t < touchCount; t++ {
		if s.has(t) {
			names = append(names, t.String())
		}
	}
	return strings.Join(names, " ")
}

// layer is where a test sits in the suite's shape.
type layer int

const (
	unit layer = iota
	integration
	e2e
	layerCount
)

var layerNames = [layerCount]string{unit: "unit", integration: "integration", e2e: "e2e"}

func (l layer) String() string { return layerNames[l] }

// layerOf is the layer rule, stated once. [LAW:single-enforcer] The table, the
// listing, and the tests all classify through it.
//
//   - end-to-end: the test runs the CLI;
//   - integration: otherwise it reaches git, a store on disk, another
//     process, or a wait on the real clock;
//   - unit: none of those. Touching the filesystem or reading the clock
//     leaves a test unit; the table counts both so they stay visible.
func layerOf(s touchSet) layer {
	switch {
	case s.has(cli):
		return e2e
	case s.any(git, store, process, clockWait):
		return integration
	default:
		return unit
	}
}
