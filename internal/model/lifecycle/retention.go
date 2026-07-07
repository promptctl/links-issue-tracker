package lifecycle

import (
	"fmt"
	"time"
)

// Retention is the sealed retention axis of the issue lifecycle, orthogonal to
// the activity axis (State): whether an issue is still in the flow, soft-hidden,
// or soft-removed. Exactly one variant holds at a time.
//
// [LAW:types-are-the-program] The prior encoding — two nullable timestamps —
// expressed four states where the domain has three, and the fourth
// (archived AND deleted) was the illegal state a scatter of imperative guards
// existed to forbid. As a sum, that state is unrepresentable and the guards'
// reason to exist is gone. The two-timestamp pair survives only as a private
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

// RetentionFromTimestamps decodes the two-nullable-timestamp encoding (the
// archived_at/deleted_at DB columns and JSON wire keys) into the sum. A legacy
// row carrying both timestamps decodes as Deleted — deletion dominates, because
// deleted rows are the ones excluded from rank space; the stale archive stamp
// on such a row is residue of the pre-sum encoding and is dropped.
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
