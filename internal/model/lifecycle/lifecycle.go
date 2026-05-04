// Package lifecycle defines the internal lifecycle expression primitives used
// by model.Issue. Callers outside internal/model must use the model package
// hydration, capability, and action APIs instead of importing this package.
//
// Container Progress aggregation is leaf-primitive based: AllOf.Progress folds
// over Progresses(a), which walks through Containers and collects every
// non-Container descendant's Progress. New primitive kinds that need to
// contribute to Progress should be leaf primitives or should implement
// Container so traversal reaches their children. Adding a wrapper primitive
// without Container semantics will make it contribute only its own Progress.
package lifecycle

import (
	"fmt"
	"strings"
)

type State string

const (
	Open       State = "open"
	InProgress State = "in_progress"
	Closed     State = "closed"
)

type Progress struct {
	Open       int `json:"open"`
	InProgress int `json:"in_progress"`
	Closed     int `json:"closed"`
	Total      int `json:"total"`
}

type ActionName string

const (
	ActionStart  ActionName = "start"
	ActionDone   ActionName = "done"
	ActionClose  ActionName = "close"
	ActionReopen ActionName = "reopen"
)

type Lifecycle interface {
	State() State
	Progress() Progress
}

// Container marks lifecycle combinators that own child lifecycle expressions.
// [LAW:one-type-per-behavior] Recursive traversal depends on one container contract instead of ad hoc structural assertions per combinator.
type Container interface {
	Lifecycle
	Children() []Lifecycle
}

type Actionable interface {
	Lifecycle
	AvailableActions() []ActionName
	Apply(name ActionName, actor string, reason string) (Lifecycle, error)
}

// Walk visits the lifecycle tree depth-first. Recursion is the substrate;
// the model package's capability and action APIs deliberately do NOT use
// Walk because they are root-only by policy. Only call Walk when you know
// you want full-tree traversal, such as Progresses for progress aggregation.
// [LAW:dataflow-not-control-flow] Tree traversal is one primitive that receives variable lifecycle data instead of scattering recursive special cases across callers.
func Walk(l Lifecycle, visit func(Lifecycle) bool) {
	if l == nil || !visit(l) {
		return
	}
	if container, ok := l.(Container); ok {
		for _, child := range container.Children() {
			Walk(child, visit)
		}
	}
}

func ParseState(value string) (State, error) {
	normalized := strings.TrimSpace(strings.ToLower(value))
	if normalized == "in-progress" {
		normalized = "in_progress"
	}
	switch State(normalized) {
	case Open, InProgress, Closed:
		return State(normalized), nil
	default:
		return "", fmt.Errorf("invalid status %q (valid: open, in_progress, closed)", value)
	}
}

// DefaultOpen parses a state, defaulting to Open for blank or unrecognized
// input. Use this for lenient boundaries (import, hydration, storage) where
// the data may be absent or legacy. Strict boundaries (CLI flags, query
// language) should use ParseState directly.
func DefaultOpen(value string) State {
	state, err := ParseState(value)
	if err != nil {
		return Open
	}
	return state
}

func ParseAction(value string) (ActionName, error) {
	normalized := strings.TrimSpace(strings.ToLower(value))
	switch ActionName(normalized) {
	case ActionStart, ActionDone, ActionClose, ActionReopen:
		return ActionName(normalized), nil
	default:
		return "", fmt.Errorf("unsupported lifecycle action %q", value)
	}
}

// Progresses collects every non-Container Progress reachable from l by walking
// through Containers. Used by AllOf.Progress to aggregate container progress;
// see the package doc for the leaf-primitive aggregation contract.
func Progresses(l Lifecycle) []Progress {
	out := []Progress{}
	Walk(l, func(current Lifecycle) bool {
		if _, ok := current.(Container); ok {
			return true
		}
		out = append(out, current.Progress())
		return true
	})
	return out
}
