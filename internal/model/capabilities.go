package model

import (
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model/lifecycle"
)

// Capabilities reports optional behavior exposed by an issue's root lifecycle
// primitive. To add a capability kind: define a view DTO, add an optional field
// here, then extend capabilitiesFrom's root switch.
type Capabilities struct {
	Status *StatusView `json:"status,omitempty"`
}

// StatusView is the flat projection of a leaf status primitive — the boundary
// representation persistence and CLI output consume. Assignee is NOT here:
// ownership is an issue-level field orthogonal to the status state machine, not
// a status-capability field. [LAW:decomposition]
type StatusView struct {
	Value    State      `json:"value"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
}

// capabilitiesFrom is root-only by design: it inspects the root lifecycle
// primitive only and does not recurse into Containers. Adding capability
// kinds means extending this function, not walking deeper.
// [LAW:one-source-of-truth] Capability presence is derived from the root lifecycle primitive rather than duplicated issue-type checks.
func capabilitiesFrom(l lifecycle.Lifecycle) Capabilities {
	if status, ok := l.(lifecycle.StatusPrimitive); ok {
		return Capabilities{Status: &StatusView{
			Value:    status.State(),
			ClosedAt: status.ClosedAt(),
		}}
	}
	return Capabilities{}
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
