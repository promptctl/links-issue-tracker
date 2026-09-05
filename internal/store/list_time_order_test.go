package store

import (
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The two timestamps this test crafts. Their temporal order is the reverse of
// their byte order, which is the whole reason the pair is worth writing down:
// RFC3339Nano trims trailing zeros, so the earlier instant produces the SHORTER
// string, and at the character where one ends the other is still going, "Z"
// (0x5A) outranks any digit. 0.12s is 3ms before 0.123456789s; ".12Z" sorts
// after ".123456789Z".
const (
	earlierInstantLaterString = "2026-01-01T00:00:00.12Z"
	laterInstantEarlierString = "2026-01-01T00:00:00.123456789Z"
)

// TestListOrdersTimestampsByInstantNotEncoding pins that this engine orders a
// timestamp key by the instant it denotes, not by the text the column happens
// to hold.
//
// This engine keeps created_at and updated_at in a varchar(64) as RFC3339Nano,
// so for as long as a listing was ordered by SQL the key was the string, and
// the pair above ordered backwards — disagreeing with wall-clock order and with
// the memory engine, which holds time.Time and always compared instants. The
// listing's order now reads hydrated model values, which is what closed that.
//
// It lives here rather than in the conformance suite because the suite reaches
// an engine only through storage.Store, which has no clock seam: no input
// carries a timestamp and the memory engine declines test support, so the
// collision cannot be constructed through the contract. Left to a real
// nanosecond clock it arises about once in ten million pairs, so a conformance
// case would pass against the very bug it named. The exposure was never
// symmetric anyway — only an engine that round-trips an instant through text
// can sort one by its spelling. [LAW:behavior-not-structure]
func TestListOrdersTimestampsByInstantNotEncoding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	earlier, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "earlier instant", Topic: "clock"})
	if err != nil {
		t.Fatalf("CreateIssue(earlier) error = %v", err)
	}
	later, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "later instant", Topic: "clock"})
	if err != nil {
		t.Fatalf("CreateIssue(later) error = %v", err)
	}

	// Stamped directly, because the collision is not something a clock can be
	// asked for. Both timestamp columns carry the pair so neither sort key is
	// pinned by accident of the other.
	for _, row := range []struct {
		id    string
		stamp string
	}{
		{earlier.ID, earlierInstantLaterString},
		{later.ID, laterInstantEarlierString},
	} {
		if _, err := st.db.ExecContext(ctx,
			"UPDATE issues SET created_at = ?, updated_at = ? WHERE id = ?", row.stamp, row.stamp, row.id); err != nil {
			t.Fatalf("stamping %s error = %v", row.id, err)
		}
	}

	for _, field := range []string{"created_at", "updated_at"} {
		assertListOrder(t, ctx, st, field, false, []string{earlier.ID, later.ID})
		assertListOrder(t, ctx, st, field, true, []string{later.ID, earlier.ID})
	}
}

func assertListOrder(t *testing.T, ctx context.Context, st *Store, field string, desc bool, want []string) {
	t.Helper()
	issues, err := st.ListIssues(ctx, storage.ListIssuesFilter{SortBy: []storage.SortSpec{{Field: field, Desc: desc}}})
	if err != nil {
		t.Fatalf("ListIssues(sort %s desc=%v) error = %v", field, desc, err)
	}
	got := make([]string, 0, len(issues))
	for _, issue := range issues {
		got = append(got, issue.ID)
	}
	direction := "ascending"
	if desc {
		direction = "descending"
	}
	if len(got) != len(want) {
		t.Fatalf("sort %s %s returned %d issues, want %d: %v", field, direction, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sort %s %s = %v, want %v (ordered by the stored text, not the instant)", field, direction, got, want)
		}
	}
}
