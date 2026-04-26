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

func (o OwnedStatus) AvailableActions() []ActionName {
	switch o.Value {
	case Open:
		return []ActionName{ActionStart, ActionClose}
	case InProgress:
		return []ActionName{ActionDone, ActionClose}
	case Closed:
		return []ActionName{ActionReopen}
	default:
		return nil
	}
}

func (o OwnedStatus) Apply(name ActionName, actor string, reason string) (Lifecycle, error) {
	next := o
	now := time.Now().UTC()
	switch name {
	case ActionStart:
		if o.Value != Open {
			return nil, fmt.Errorf("no %s action available on this issue", name)
		}
		next.Value = InProgress
	case ActionDone:
		if o.Value != InProgress {
			return nil, fmt.Errorf("no %s action available on this issue", name)
		}
		next.Value = Closed
		next.ClosedAt = &now
	case ActionClose:
		if o.Value == Closed {
			return nil, fmt.Errorf("no %s action available on this issue", name)
		}
		next.Value = Closed
		next.ClosedAt = &now
	case ActionReopen:
		if o.Value != Closed {
			return nil, fmt.Errorf("no %s action available on this issue", name)
		}
		next.Value = Open
		next.ClosedAt = nil
	default:
		return nil, fmt.Errorf("unsupported lifecycle action %q", name)
	}
	return next, nil
}
