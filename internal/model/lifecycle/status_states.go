package lifecycle

import (
	"fmt"
	"time"
)

// The leaf lifecycle of a single issue is a sum type: one variant per state.
// Each variant carries exactly the fields meaningful in that state — which is
// none for open and in_progress, and the close timestamp for closed.
//
// [LAW:types-are-the-program] A close timestamp on a non-closed state is
// unrepresentable: only closedState has the field, so the illegal state cannot
// be constructed. Ownership (assignee) is deliberately absent here — it is
// orthogonal to the status state machine and lives on the issue itself, not in
// the lifecycle. [LAW:decomposition]

// StatusPrimitive is the sealed leaf lifecycle carrying a single issue's status.
// Its only implementations are the three state variants below; AllOf (a
// container, whose state derives from children) is deliberately not one, since
// it does not satisfy Actionable.
// [LAW:one-type-per-behavior] One contract identifies "this lifecycle is an
// own-able leaf status" so the projection boundary discriminates on it instead
// of enumerating concrete variants.
type StatusPrimitive interface {
	Actionable
	// ClosedAt is the close timestamp, present only on the closed state and nil
	// on every other. The projection seam reads it without knowing the variant.
	ClosedAt() *time.Time
	// Resolution is the close reason, present only on the closed state and nil
	// on every other. Like ClosedAt it is read through the seam without the
	// caller discriminating the variant; on a non-closed state it is structurally
	// absent, so a resolution on open/in_progress is unrepresentable.
	Resolution() *Resolution
}

type openState struct{}

func (openState) State() State       { return Open }
func (openState) Progress() Progress { return Progress{Open: 1, Total: 1} }
func (openState) ClosedAt() *time.Time {
	return nil
}
func (openState) Resolution() *Resolution {
	return nil
}
func (o openState) Apply(name ActionName, actor string, reason string) (Lifecycle, error) {
	return applyStatusAction(o, name)
}

type inProgressState struct{}

func (inProgressState) State() State       { return InProgress }
func (inProgressState) Progress() Progress { return Progress{InProgress: 1, Total: 1} }
func (inProgressState) ClosedAt() *time.Time {
	return nil
}
func (inProgressState) Resolution() *Resolution {
	return nil
}
func (s inProgressState) Apply(name ActionName, actor string, reason string) (Lifecycle, error) {
	return applyStatusAction(s, name)
}

type closedState struct {
	// closedAt is a pointer because legacy rows and field-wise merges can settle
	// on a closed state without a known timestamp; the strongest TRUE theorem is
	// "closed may carry a close time," not "closed always has one".
	closedAt *time.Time
	// resolution is a pointer for the same reason closedAt is: a `done` close, a
	// legacy row, or a field-wise merge can settle on closed with no resolution.
	// The strongest TRUE theorem is "closed may carry a resolution"; `lit close`
	// requires one at its command boundary, but the type does not.
	resolution *Resolution
}

func (closedState) State() State       { return Closed }
func (closedState) Progress() Progress { return Progress{Closed: 1, Total: 1} }
func (c closedState) ClosedAt() *time.Time {
	return cloneTime(c.closedAt)
}
func (c closedState) Resolution() *Resolution {
	return cloneResolution(c.resolution)
}
func (c closedState) Apply(name ActionName, actor string, reason string) (Lifecycle, error) {
	return applyStatusAction(c, name)
}

// NewStatus builds the leaf variant for state, attaching closedAt and
// resolution only to the closed variant (ignored for the others). Blank or
// unrecognized states default to open, matching the lenient hydration boundary.
// [LAW:single-enforcer] The one place flat (state, closedAt, resolution) row
// data becomes a typed leaf lifecycle, so no caller can mint a variant with a
// field meaningless in its state.
func NewStatus(state State, closedAt *time.Time, resolution *Resolution) StatusPrimitive {
	switch DefaultOpen(string(state)) {
	case Closed:
		return closedState{closedAt: cloneTime(closedAt), resolution: cloneResolution(resolution)}
	case InProgress:
		return inProgressState{}
	default:
		return openState{}
	}
}

// applyStatusAction is the target-state transition shared by every leaf state:
// the action names the desired terminal state and we return that state's
// variant. A same-state call returns the receiver so the store recognizes it as
// a no-op. Transitioning into closed stamps the close time; every other target
// carries no timestamp, so a close time can never linger on a non-closed state.
// [LAW:one-source-of-truth] The action→target mapping is read from
// ActionTargetState; this function maintains no parallel table.
func applyStatusAction(current Lifecycle, name ActionName) (Lifecycle, error) {
	target, ok := ActionTargetState(name)
	if !ok {
		return nil, fmt.Errorf("unsupported lifecycle action %q", name)
	}
	if current.State() == target {
		return current, nil
	}
	switch target {
	case Closed:
		now := time.Now().UTC()
		return closedState{closedAt: &now}, nil
	case InProgress:
		return inProgressState{}, nil
	default:
		return openState{}, nil
	}
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
