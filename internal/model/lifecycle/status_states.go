package lifecycle

import (
	"strings"
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
	// RedirectTarget is the canonical ticket a duplicate/superseded close
	// redirects to, present only on a closed state whose resolution redirects
	// and nil on every other. The target is the redirecting resolution's
	// payload: NewStatus attaches it only beside a redirecting resolution, so a
	// target on a terminal or absent resolution is unrepresentable.
	RedirectTarget() *string
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
func (openState) RedirectTarget() *string {
	return nil
}
func (o openState) Apply(action StatusAction) Lifecycle {
	return applyStatusAction(o, action)
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
func (inProgressState) RedirectTarget() *string {
	return nil
}
func (s inProgressState) Apply(action StatusAction) Lifecycle {
	return applyStatusAction(s, action)
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
	// redirectTarget is the canonical ticket a redirecting resolution points to.
	// It is a pointer because a legacy redirecting close (backfill-ambiguous, or
	// pre-column) may carry no recoverable target; the strongest TRUE theorem is
	// "a redirecting close may carry its target". The store's close-validation
	// floor requires one for every NEW redirecting close, and NewStatus refuses
	// a target beside a non-redirecting resolution, so the pairings this type
	// leaves representable are exactly the legal ones.
	redirectTarget *string
}

func (closedState) State() State       { return Closed }
func (closedState) Progress() Progress { return Progress{Closed: 1, Total: 1} }
func (c closedState) ClosedAt() *time.Time {
	return cloneTime(c.closedAt)
}
func (c closedState) Resolution() *Resolution {
	return cloneResolution(c.resolution)
}
func (c closedState) RedirectTarget() *string {
	return cloneString(c.redirectTarget)
}
func (c closedState) Apply(action StatusAction) Lifecycle {
	return applyStatusAction(c, action)
}

// NewStatus builds the leaf variant for state, attaching closedAt, resolution,
// and redirectTarget only to the closed variant (ignored for the others). Blank
// or unrecognized states default to open, matching the lenient hydration
// boundary. The redirect target attaches only beside a redirecting resolution —
// the target is that resolution's payload — so a target on a terminal or absent
// resolution is dropped here and cannot reach a leaf.
// [LAW:single-enforcer] The one place flat (state, closedAt, resolution,
// redirectTarget) row data becomes a typed leaf lifecycle, so no caller can
// mint a variant with a field meaningless in its state.
func NewStatus(state State, closedAt *time.Time, resolution *Resolution, redirectTarget *string) StatusPrimitive {
	switch DefaultOpen(string(state)) {
	case Closed:
		if resolution == nil || !resolution.RedirectsToCanonical() {
			redirectTarget = nil
		}
		return closedState{closedAt: cloneTime(closedAt), resolution: cloneResolution(resolution), redirectTarget: cloneString(redirectTarget)}
	case InProgress:
		return inProgressState{}
	default:
		return openState{}
	}
}

// applyStatusAction is the target-state transition shared by every leaf state:
// the action names the desired terminal state and we return that state's
// variant. A same-state call returns the receiver so the store recognizes it as
// a no-op — including a re-close, which keeps the existing resolution rather
// than silently rewriting it. Transitioning into closed stamps the close time
// and attaches the action's outcome — resolution and redirect target both — so
// a close's payload travels through the machine instead of being re-attached
// after it; every other target carries none of them, so a close time,
// resolution, or redirect target can never linger on a non-closed state.
// It cannot fail: Target is total over StatusAction, so the old
// "unsupported lifecycle action" arm is unrepresentable.
// [LAW:one-source-of-truth] The action→target mapping is the variant's Target;
// this function maintains no parallel table.
func applyStatusAction(current Lifecycle, action StatusAction) Lifecycle {
	target := action.Target()
	if current.State() == target {
		return current
	}
	switch target {
	case Closed:
		now := time.Now().UTC()
		return closedState{closedAt: &now, resolution: closeResolution(action), redirectTarget: closeRedirectTarget(action)}
	case InProgress:
		return inProgressState{}
	default:
		return openState{}
	}
}

// closeResolution projects the resolution a closing action records: Close
// carries its outcome's resolution; Done is the neutral success close and
// records none. [LAW:dataflow-not-control-flow] The variant is the value the
// one transition varies on. A Close minted without an outcome is a
// constructor bug, and it panics here the same way impostor Retention values
// do — loud at the source, never nil-guarded at readers. [LAW:no-silent-failure]
func closeResolution(action StatusAction) *Resolution {
	if c, ok := action.(Close); ok {
		if c.Outcome == nil {
			panic("lifecycle: Close action requires an Outcome; use Done for the neutral success close")
		}
		r := c.Outcome.Resolution()
		return &r
	}
	return nil
}

// closeRedirectTarget projects the redirect target a closing action records:
// only the redirecting Outcome variants carry one, read structurally off the
// variant's field. The value is normalized here — trimmed, with empty as nil —
// so "a redirecting close whose target is unknown" has exactly one
// representation (nil) for the store's validation floor to key on.
// [LAW:dataflow-not-control-flow] Like closeResolution, the variant is the
// value the one transition varies on.
func closeRedirectTarget(action StatusAction) *string {
	c, ok := action.(Close)
	if !ok {
		return nil
	}
	var target string
	switch o := c.Outcome.(type) {
	case Duplicate:
		target = o.Of
	case Superseded:
		target = o.By
	default:
		return nil
	}
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
