package model

import (
	"time"

	"github.com/bmf/links-issue-tracker/internal/model/lifecycle"
)

// Capabilities reports optional behavior exposed by an issue's root lifecycle
// primitive. To add a capability kind: define a view DTO, add an optional field
// here, then extend capabilitiesFrom's root switch.
type Capabilities struct {
	Status *StatusView `json:"status,omitempty"`
}

type StatusView struct {
	Value    State      `json:"value"`
	Assignee string     `json:"assignee,omitempty"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
}

// capabilitiesFrom is root-only by design: it switches on the root lifecycle
// primitive only and does not recurse into Containers. Adding capability
// kinds means extending this switch, not walking deeper.
// [LAW:one-source-of-truth] Capability presence is derived from the root lifecycle primitive rather than duplicated issue-type checks.
func capabilitiesFrom(l lifecycle.Lifecycle) Capabilities {
	switch typed := l.(type) {
	case lifecycle.OwnedStatus:
		return Capabilities{Status: &StatusView{
			Value:    State(typed.Value),
			Assignee: typed.Assignee,
			ClosedAt: cloneTime(typed.ClosedAt),
		}}
	default:
		return Capabilities{}
	}
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
