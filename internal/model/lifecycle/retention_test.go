package lifecycle

import (
	"fmt"
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

// A typed-nil pointer variant satisfies the interface but is not one of the
// three sealed value variants; the encoder must refuse it loudly rather than
// silently collapsing it to Live at a write boundary.
func TestRetentionTimestampsRefusesImpostors(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("encode((*Archived)(nil)) did not panic")
		}
	}()
	RetentionTimestamps((*Archived)(nil))
}

// TestRetainTransitionTable pins every cell of the retention state machine:
// three variants by four retention actions. The completeness check below fails
// the test if a (variant, action) pair is missing, so "total" is asserted, not
// assumed. There is no non-retention row to pin: an activity action is not a
// RetentionAction, so feeding one to Retain does not compile.
func TestRetainTransitionTable(t *testing.T) {
	prior := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	at := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	live := Live{}
	archived := Archived{At: prior}
	deleted := Deleted{At: prior}

	cases := []struct {
		cur     Retention
		action  RetentionAction
		want    Retention // nil means the transition is illegal
		wantErr string
	}{
		{live, Archive{}, Archived{At: at}, ""},
		{archived, Archive{}, nil, "issue is already archived"},
		{deleted, Archive{}, nil, "cannot archive deleted issue"},

		{live, Unarchive{}, nil, "issue is not archived"},
		{archived, Unarchive{}, Live{}, ""},
		{deleted, Unarchive{}, nil, "cannot unarchive deleted issue"},

		{live, Delete{}, Deleted{At: at}, ""},
		// The decided delete-on-archived behavior: Deleted carries no
		// prior-archived bit, so the archive stamp is gone at the type level.
		{archived, Delete{}, Deleted{At: at}, ""},
		{deleted, Delete{}, nil, "issue is already deleted"},

		{live, Restore{}, nil, "issue is not deleted"},
		{archived, Restore{}, nil, "issue is not deleted"},
		{deleted, Restore{}, Live{}, ""},
	}

	covered := map[string]bool{}
	for _, tc := range cases {
		covered[fmt.Sprintf("%T/%T", tc.cur, tc.action)] = true
		got, err := Retain(tc.cur, tc.action, at)
		if tc.wantErr != "" {
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("Retain(%#v, %T) error = %v, want %q", tc.cur, tc.action, err, tc.wantErr)
			}
			if got != nil {
				t.Fatalf("Retain(%#v, %T) returned %#v alongside an error", tc.cur, tc.action, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Retain(%#v, %T) error = %v, want %#v", tc.cur, tc.action, err, tc.want)
		}
		if got != tc.want {
			t.Fatalf("Retain(%#v, %T) = %#v, want %#v", tc.cur, tc.action, got, tc.want)
		}
	}
	for _, cur := range []Retention{live, archived, deleted} {
		for _, action := range []RetentionAction{Archive{}, Unarchive{}, Delete{}, Restore{}} {
			if !covered[fmt.Sprintf("%T/%T", cur, action)] {
				t.Fatalf("transition table has no row for (%T, %T)", cur, action)
			}
		}
	}
}

// Pins the epic-decided lifecycle sequence: archive → delete → restore lands
// on Live, never back on Archived — Deleted remembers nothing.
func TestRetainArchiveDeleteRestoreLandsLive(t *testing.T) {
	at := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	var cur Retention = Live{}
	for _, action := range []RetentionAction{Archive{}, Delete{}, Restore{}} {
		next, err := Retain(cur, action, at)
		if err != nil {
			t.Fatalf("Retain(%#v, %T) error = %v", cur, action, err)
		}
		cur = next
	}
	if cur != (Live{}) {
		t.Fatalf("archive→delete→restore ended at %#v, want Live", cur)
	}
}

func TestRetainRefusesImpostors(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("Retain((*Deleted)(nil), archive) did not panic")
		}
	}()
	_, _ = Retain((*Deleted)(nil), Archive{}, time.Now())
}

// A typed-nil pointer variant satisfies RetentionAction without being one of
// the four sealed value variants; the machine must refuse it loudly rather
// than silently skipping the transition.
func TestRetainRefusesImpostorActions(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("Retain(Live, (*Archive)(nil)) did not panic")
		}
	}()
	_, _ = Retain(Live{}, (*Archive)(nil), time.Now())
}

func TestFrozen(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		r    Retention
		want bool
	}{
		{Live{}, false},
		{Archived{At: at}, true},
		{Deleted{At: at}, true},
	} {
		if got := Frozen(tc.r); got != tc.want {
			t.Fatalf("Frozen(%#v) = %v, want %v", tc.r, got, tc.want)
		}
	}
}

func TestFrozenRefusesImpostors(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("Frozen((*Archived)(nil)) did not panic")
		}
	}()
	Frozen((*Archived)(nil))
}
