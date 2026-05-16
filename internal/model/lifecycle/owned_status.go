package lifecycle

import (
	"fmt"
	"time"
)

type OwnedStatus struct {
	Value    State
	Assignee string
	ClosedAt *time.Time
}

func (o OwnedStatus) State() State {
	return o.Value
}

func (o OwnedStatus) Progress() Progress {
	progress := Progress{Total: 1}
	switch o.Value {
	case Closed:
		progress.Closed = 1
	case InProgress:
		progress.InProgress = 1
	default:
		progress.Open = 1
	}
	return progress
}

// Apply implements the target-state lifecycle model: the action declares the
// desired terminal state, and Apply produces it. Same-state inputs return the
// receiver unchanged so the store layer can recognize the call as a no-op (no
// status field-change, no audit drift on ClosedAt). The only rejection is
// ParseAction-bypass — a value that is not one of the four known actions.
// [LAW:types-are-the-program] All from-state preconditions are gone; the only
// constraint left is "action must name a known target state."
// [LAW:one-source-of-truth] The action→target mapping is read from
// ActionTargetState; this function does not maintain a parallel table.
func (o OwnedStatus) Apply(name ActionName, actor string, reason string) (Lifecycle, error) {
	target, ok := ActionTargetState(name)
	if !ok {
		return nil, fmt.Errorf("unsupported lifecycle action %q", name)
	}
	if o.Value == target {
		return o, nil
	}
	next := o
	next.Value = target
	switch target {
	case Closed:
		now := time.Now().UTC()
		next.ClosedAt = &now
	case Open:
		next.ClosedAt = nil
	}
	return next, nil
}
