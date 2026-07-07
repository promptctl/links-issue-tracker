package lifecycle

import (
	"testing"
	"time"
)

func TestRetentionFromTimestamps(t *testing.T) {
	archived := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	deleted := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

	if _, ok := RetentionFromTimestamps(nil, nil).(Live); !ok {
		t.Fatalf("decode(nil, nil) = %#v, want Live", RetentionFromTimestamps(nil, nil))
	}
	if got, ok := RetentionFromTimestamps(&archived, nil).(Archived); !ok || !got.At.Equal(archived) {
		t.Fatalf("decode(archived, nil) = %#v, want Archived{%v}", RetentionFromTimestamps(&archived, nil), archived)
	}
	if got, ok := RetentionFromTimestamps(nil, &deleted).(Deleted); !ok || !got.At.Equal(deleted) {
		t.Fatalf("decode(nil, deleted) = %#v, want Deleted{%v}", RetentionFromTimestamps(nil, &deleted), deleted)
	}
	// A legacy row carrying both stamps decodes as Deleted: deletion dominates,
	// and the stale archive stamp is residue of the pre-sum encoding.
	if got, ok := RetentionFromTimestamps(&archived, &deleted).(Deleted); !ok || !got.At.Equal(deleted) {
		t.Fatalf("decode(both) = %#v, want Deleted{%v}", RetentionFromTimestamps(&archived, &deleted), deleted)
	}
}

func TestRetentionTimestampsRoundTrip(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, r := range []Retention{Live{}, Archived{At: at}, Deleted{At: at}} {
		archivedAt, deletedAt := RetentionTimestamps(r)
		// [LAW:types-are-the-program] The encoder's input cannot represent
		// archived-and-deleted, so the pair it emits never has both set.
		if archivedAt != nil && deletedAt != nil {
			t.Fatalf("encode(%#v) emitted both timestamps", r)
		}
		if got := RetentionFromTimestamps(archivedAt, deletedAt); got != r {
			t.Fatalf("round-trip(%#v) = %#v", r, got)
		}
	}
}
