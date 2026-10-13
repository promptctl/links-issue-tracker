package lifecycle

import (
	"errors"
	"fmt"
	"time"
)

// Retention is the sealed retention axis of the issue lifecycle, orthogonal to
// the activity axis (State): whether an issue is still in the flow, soft-hidden,
// or soft-removed. Exactly one variant holds at a time.
//
// [LAW:types-are-the-program] As a sum, archived AND deleted is
// unrepresentable. The two-timestamp pair survives only as a private
// wire/storage encoding behind the encoder/decoder below.
type Retention interface{ isRetention() }

// Live is the retention origin: neither archived nor deleted. It is the
// meaning assigned to the zero value of the axis.
type Live struct{}

// Archived is soft-hidden since At: out of the default listings but retained
// in rank space, reversible via unarchive.
type Archived struct{ At time.Time }

// Deleted is soft-removed since At: excluded from rank space, reversible via
// restore.
type Deleted struct{ At time.Time }

func (Live) isRetention()     {}
func (Archived) isRetention() {}
func (Deleted) isRetention()  {}

// Frozen reports whether r has left the flow: archived and deleted issues are
// read-only for activity transitions and invisible to pending-work traversal
// until a retention transition returns them to Live.
// [LAW:single-enforcer] The one predicate consumers ask instead of matching
// retention variants themselves; it reads Thaw, so "frozen" and "has a way
// back" cannot disagree.
func Frozen(r Retention) bool {
	_, frozen := Thaw(r)
	return frozen
}

// Thaw is the retention action that returns r to Live, and false when r is
// already Live. A frozen issue is refused every activity transition, so the
// refusal has to name this action: it is the one move that makes the refused
// command legal again.
// [LAW:one-source-of-truth] The pairing is the inverse of Retain's table, and a
// test holds it there: Retain(r, Thaw(r)) is Live for every frozen variant.
func Thaw(r Retention) (RetentionAction, bool) {
	switch r.(type) {
	case Live:
		return nil, false
	case Archived:
		return Unarchive{}, true
	case Deleted:
		return Restore{}, true
	default:
		// [LAW:no-silent-failure] Same refusal as RetentionTimestamps: an
		// impostor answered either way would silently mislabel the issue.
		panic(fmt.Sprintf("illegal Retention value %T", r))
	}
}

// RetentionName is the one rendering of a retention variant as a word. Display
// surfaces name the state an issue is in; without a single namer each surface
// would match the variants itself and they would drift the day a fourth variant
// arrives. [LAW:one-source-of-truth] Consumers ask this instead of matching
// retention variants for their own labels, exactly as they ask Frozen instead of
// matching them for the out-of-the-flow question.
func RetentionName(r Retention) string {
	switch r.(type) {
	case Live:
		return "live"
	case Archived:
		return "archived"
	case Deleted:
		return "deleted"
	default:
		// [LAW:no-silent-failure] Same refusal as Frozen: an impostor named
		// either way would mislabel the issue on every surface that reads this.
		panic(fmt.Sprintf("illegal Retention value %T", r))
	}
}

// Retain is the total retention-transition function: the Retention that
// applying action to cur at time at yields, or the reason the move is illegal.
// Pure — no clock, no store; the caller supplies the stamp time.
// [LAW:types-are-the-program] The whole retention state machine lives in this
// one table. Every (variant, action) cell is written out, so a rejection is an
// exhaustive match arm here, never an imperative guard at a callsite. The
// action arrives as the sealed RetentionAction subset, so an activity action
// or arbitrary verb in this machine is unconstructible — there is no
// unsupported-action row because its input cannot be expressed.
func Retain(cur Retention, action RetentionAction, at time.Time) (Retention, error) {
	switch action.(type) {
	case Archive:
		switch cur.(type) {
		case Live:
			return Archived{At: at}, nil
		case Archived:
			return nil, errors.New("issue is already archived")
		case Deleted:
			return nil, errors.New("cannot archive deleted issue")
		}
	case Unarchive:
		switch cur.(type) {
		case Live:
			return nil, errors.New("issue is not archived")
		case Archived:
			return Live{}, nil
		case Deleted:
			return nil, errors.New("cannot unarchive deleted issue")
		}
	case Delete:
		switch cur.(type) {
		case Live, Archived:
			// Deleted carries no prior-archived bit — deleting an archived
			// issue drops the archive stamp by construction, so restore always
			// lands on Live.
			return Deleted{At: at}, nil
		case Deleted:
			return nil, errors.New("issue is already deleted")
		}
	case Restore:
		switch cur.(type) {
		case Live, Archived:
			return nil, errors.New("issue is not deleted")
		case Deleted:
			return Live{}, nil
		}
	default:
		// [LAW:no-silent-failure] Only the four sealed value variants are legal;
		// an impostor (typed-nil pointer variant) must refuse loudly rather than
		// silently skip the transition.
		panic(fmt.Sprintf("illegal RetentionAction value %T", action))
	}
	// [LAW:no-silent-failure] Every sealed value variant returned above; only
	// an impostor Retention (typed-nil pointer variant, raw nil) reaches here.
	panic(fmt.Sprintf("illegal Retention value %T", cur))
}

// RetentionFromTimestamps decodes the two-nullable-timestamp encoding (the
// archived_at/deleted_at DB columns and JSON wire keys) into the sum. A legacy
// row carrying both timestamps decodes as Deleted — deletion dominates, because
// deleted rows are the ones excluded from rank space; the stale archive stamp
// on such a row is dropped.
// [LAW:single-enforcer] The one place the pair becomes a Retention; every read
// boundary (row scan, shape map, JSON) folds through here.
func RetentionFromTimestamps(archivedAt, deletedAt *time.Time) Retention {
	switch {
	case deletedAt != nil:
		return Deleted{At: *deletedAt}
	case archivedAt != nil:
		return Archived{At: *archivedAt}
	default:
		return Live{}
	}
}

// RetentionTimestamps encodes a Retention back into the two-nullable-timestamp
// pair. Its input cannot represent archived-and-deleted, so no writer fed by
// this function can produce the both-set state.
// [LAW:single-enforcer] The one place a Retention becomes the pair; every write
// boundary (SQL columns, JSON) projects through here.
func RetentionTimestamps(r Retention) (archivedAt, deletedAt *time.Time) {
	switch v := r.(type) {
	case Live:
		return nil, nil
	case Archived:
		at := v.At
		return &at, nil
	case Deleted:
		at := v.At
		return nil, &at
	default:
		// [LAW:no-silent-failure] Only the three sealed value variants are legal.
		// Go still admits impostors — a typed-nil pointer variant, or nil when a
		// caller bypasses the Issue accessor's zero-value normalization — and a
		// catch-all here would silently collapse them to Live at every write
		// boundary. Refuse loudly at the one consumer of the sum's structure.
		panic(fmt.Sprintf("illegal Retention value %T", r))
	}
}
