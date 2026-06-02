package store

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// normEvent is an order-insensitive, provenance-insensitive view of one issue
// event — the shape both the live reconcile and the lifeboat fan-out must agree
// on for the translation to be conserved. Timestamps are formatted to a canonical
// string so two equal instants compare equal regardless of location.
type normEvent struct {
	IssueID   string
	Action    string
	Reason    string
	Actor     string
	CreatedAt string
	Changes   []model.FieldChange
}

func normalizeEvents(events []model.IssueEvent, keep func(id string) bool) map[string]normEvent {
	out := map[string]normEvent{}
	for _, ev := range events {
		if !keep(ev.ID) {
			continue
		}
		changes := append([]model.FieldChange(nil), ev.Changes...)
		sort.Slice(changes, func(i, j int) bool {
			if changes[i].Field != changes[j].Field {
				return changes[i].Field < changes[j].Field
			}
			if changes[i].From != changes[j].From {
				return changes[i].From < changes[j].From
			}
			return changes[i].To < changes[j].To
		})
		out[ev.ID] = normEvent{
			IssueID:   ev.IssueID,
			Action:    ev.Action,
			Reason:    ev.Reason,
			Actor:     ev.Actor,
			CreatedAt: ev.CreatedAt.UTC().Format(time.RFC3339Nano),
			Changes:   changes,
		}
	}
	return out
}

// TestFanOutConservesIssueHistoryAgainstReconcile is the acceptance: the
// issue_history fan-out ShapeMapping, applied to a raw dump, produces exactly the
// issue_events (+ conditional issue_event_changes) the FROZEN reconcile
// (translateIssueHistoryToEvents) produces for the same rows. The reconcile is
// the trusted oracle; the lifeboat must conserve every translatable history row —
// every event field, and a status change row for exactly the real transitions.
//
// The dump is taken BELOW the migration gate, before reconcile runs, so the two
// paths consume the identical legacy rows: one through the in-place forward
// migration, one through dump→map→apply.
func TestFanOutConservesIssueHistoryAgainstReconcile(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open(first): %v", err)
	}
	seeded, err := first.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "Has history", Topic: "history", IssueType: "task", Priority: 0})
	if err != nil {
		_ = first.Close()
		t.Fatalf("seed CreateIssue: %v", err)
	}
	seedCanonicalIssueHistory(t, first)
	// The same rich row set the reconcile's own acceptance pins: named/NULL/empty
	// actions, real and non transitions, whitespace, and legacy status spellings.
	insertLegacyHistory(t, first, "hist-start", seeded.ID, strPtr("start"), "began work", strPtr("open"), strPtr("in_progress"), "2026-01-01T10:00:00Z", "alice")
	insertLegacyHistory(t, first, "hist-comment-null", seeded.ID, nil, "added context", nil, nil, "2026-01-01T10:05:00Z", "alice")
	insertLegacyHistory(t, first, "hist-comment-empty", seeded.ID, strPtr(""), "added more context", nil, nil, "2026-01-01T10:06:00Z", "alice")
	insertLegacyHistory(t, first, "hist-close", seeded.ID, strPtr("close"), "shipped", strPtr("in_progress"), strPtr("closed"), "2026-01-01T11:00:00Z", "bob")
	insertLegacyHistory(t, first, "hist-same-status", seeded.ID, strPtr("touch"), "no movement", strPtr("closed"), strPtr("closed"), "2026-01-01T11:30:00Z", "bob")
	insertLegacyHistory(t, first, "hist-whitespace", seeded.ID, strPtr("  start  "), "  padded reason  ", nil, nil, "2026-01-01T12:00:00Z", "  carol  ")
	if err := first.ExecRawForTest(ctx, "ALTER TABLE issues DROP CHECK issues_status_check"); err != nil {
		_ = first.Close()
		t.Fatalf("drop status check: %v", err)
	}
	insertLegacyHistory(t, first, "hist-legacy-transition", seeded.ID, strPtr("close"), "legacy close", strPtr("todo"), strPtr("done"), "2026-01-01T12:30:00Z", "carol")
	insertLegacyHistory(t, first, "hist-legacy-nontransition", seeded.ID, strPtr("touch"), "spelling differs only", strPtr("in-progress"), strPtr("in_progress"), "2026-01-01T12:45:00Z", "carol")
	hijackToPreGoose(t, first)
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first): %v", err)
	}

	// Path B (lifeboat): dump below the gate, fan the issue_history table out.
	dump, err := DumpRaw(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("DumpRaw: %v", err)
	}
	history, ok := tableFromDump(dump, "issue_history")
	if !ok {
		t.Fatal("dump did not carry issue_history")
	}
	ihDump := RawDump{WorkspaceID: dump.WorkspaceID, Tables: []RawTable{history}}
	mapping, ok := DeterministicMap(ihDump)
	if !ok {
		t.Fatal("DeterministicMap declined the canonical issue_history shape")
	}
	exportB, err := Apply(ihDump, mapping)
	if err != nil {
		t.Fatalf("Apply fan-out: %v", err)
	}

	// Path A (oracle): forward-migrate in place; the reconcile translates the same
	// rows into issue_events (+ change rows) before dropping issue_history.
	ref := assertReachedBaseline(t, doltRoot)
	refExport, err := ref.Export(ctx)
	if err != nil {
		t.Fatalf("reference Export: %v", err)
	}

	isHistory := func(id string) bool { return len(id) >= 5 && id[:5] == "hist-" }
	want := normalizeEvents(refExport.Events, isHistory)
	got := normalizeEvents(exportB.Events, isHistory)

	if len(want) != 8 {
		t.Fatalf("oracle premise broken: want 8 translated history events, reconcile produced %d", len(want))
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("fan-out does not conserve the reconcile translation:\nreconcile (oracle):\n%+v\nfan-out:\n%+v", want, got)
	}
}

// tableFromDump returns the named table from a dump.
func tableFromDump(dump RawDump, name string) (RawTable, bool) {
	for _, t := range dump.Tables {
		if t.Name == name {
			return t, true
		}
	}
	return RawTable{}, false
}
