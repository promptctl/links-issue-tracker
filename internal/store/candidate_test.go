package store

import (
	"context"
	"os"
	"testing"
)

// preGooseDump is a known, valid pre-goose shape (the same vocabulary
// TestDeterministicMapPreGoose exercises): aliased columns, legacy status, no
// goose bookkeeping. DeterministicMap maps it without declining, so it is the
// fixture for "a valid mapping yields a fresh candidate".
func preGooseDump() RawDump {
	const created = "2026-01-01T00:00:00Z"
	const closed = "2026-01-02T00:00:00Z"
	return RawDump{WorkspaceID: "legacy-ws", Tables: []RawTable{
		{Name: "issues",
			Columns: []string{"id", "title", "description", "prompt", "status", "priority", "issue_type", "assignee", "created_at", "updated_at", "closed_at"},
			Rows: [][]any{
				{"i1", "Open task", "desc one", "the historical prompt", "todo", int64(0), "task", "", created, created, nil},
				{"i2", "Done task", "desc two", nil, "done", int64(0), "task", "claude", created, closed, closed},
			}},
		{Name: "relations", Columns: []string{"src_id", "dst_id", "type", "created_at", "created_by"}, Rows: [][]any{}},
		{Name: "comments", Columns: []string{"id", "issue_id", "body", "created_at", "created_by"},
			Rows: [][]any{{"c1", "i1", "a comment", created, "claude"}}},
		{Name: "labels", Columns: []string{"issue_id", "label", "created_at", "created_by"},
			Rows: [][]any{{"i1", "backend", created, "claude"}}},
		{Name: "issue_events", Columns: []string{"id", "issue_id", "action", "reason", "assignee", "created_at"},
			Rows: [][]any{{"e1", "i2", "done", "finished", "claude", closed}}},
		{Name: "issue_event_changes", Columns: []string{"event_id", "field", "from_value", "to_value"},
			Rows: [][]any{{"e1", "status", "in_progress", "closed"}}},
	}}
}

func mustMap(t *testing.T, dump RawDump) ShapeMapping {
	t.Helper()
	mapping, ok := DeterministicMap(dump)
	if !ok {
		t.Fatal("DeterministicMap declined a known shape")
	}
	return mapping
}

// dirEntryCount counts entries under dir, failing the test on a read error.
func dirEntryCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %q: %v", dir, err)
	}
	return len(entries)
}

// TestRebuildCandidateValidMappingYieldsFreshWorkspace is acceptance #1: a valid
// mapping produces a candidate at the current baseline, Doctor-clean, with every
// source issue conserved.
func TestRebuildCandidateValidMappingYieldsFreshWorkspace(t *testing.T) {
	ctx := context.Background()
	dump := preGooseDump()

	cand, err := RebuildCandidate(ctx, t.TempDir(), dump, mustMap(t, dump))
	if err != nil {
		t.Fatalf("RebuildCandidate rejected a valid mapping: %v", err)
	}
	t.Cleanup(func() { _ = cand.Discard() })

	report, err := cand.Store().Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	mustClean(t, report)

	export, err := cand.Store().Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(export.Issues) != 2 {
		t.Fatalf("issue conservation broken: want 2 issues, got %d", len(export.Issues))
	}
}

// TestRebuildCandidateRejectLeavesZeroResidue is the core new guarantee
// (acceptance #2): an invalid mapping is rejected and leaves nothing on disk, so
// the very next attempt — under the same parent dir, reusing the same dump —
// starts clean. The dump is read-only across both attempts.
func TestRebuildCandidateRejectLeavesZeroResidue(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	dump := preGooseDump()

	// An empty mapping is not total over the dump's columns: Apply rejects it.
	if _, err := RebuildCandidate(ctx, parent, dump, ShapeMapping{Columns: map[ColumnRef]Disposition{}}); err == nil {
		t.Fatal("RebuildCandidate accepted an incomplete mapping")
	}
	if n := dirEntryCount(t, parent); n != 0 {
		t.Fatalf("rejected attempt left %d entries under the parent dir; want zero residue", n)
	}

	// Same dump, now a valid mapping: the next attempt starts clean and conserves
	// every issue. If the rejected attempt had contaminated state, this count
	// would be wrong.
	cand, err := RebuildCandidate(ctx, parent, dump, mustMap(t, dump))
	if err != nil {
		t.Fatalf("RebuildCandidate rejected a valid mapping after a prior reject: %v", err)
	}
	// Always release the candidate even if an assertion below exits via Fatalf,
	// so no open store or workspace lock leaks into the rest of the package run.
	// Idempotent with the explicit Discard the zero-residue assertion uses.
	t.Cleanup(func() { _ = cand.Discard() })
	export, err := cand.Store().Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(export.Issues) != 2 {
		t.Fatalf("next attempt not clean: want 2 issues, got %d", len(export.Issues))
	}

	// Discarding a SUCCESSFUL candidate must also leave zero residue under the
	// parent — including the workspace lock and migration snapshots Open writes as
	// siblings of the dolt directory. This is the guarantee a flat dolt-dir layout
	// silently broke (those siblings escaped the candidate's own RemoveAll).
	if err := cand.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if n := dirEntryCount(t, parent); n != 0 {
		t.Fatalf("discarded candidate left %d entries under the parent dir; want zero residue", n)
	}
}

// TestDiscardRetriesDirectoryRemoval proves the zero-residue guarantee survives
// a transient filesystem failure. With the parent dir made unwritable, the first
// Discard closes the store but cannot unlink the candidate directory, so it
// errors and keeps dir tracked; once the parent is writable again a second
// Discard re-attempts removal and clears it. A single shared release flag would
// have nulled everything on the first attempt and the retry would no-op,
// stranding the directory.
func TestDiscardRetriesDirectoryRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("removal-permission injection has no effect as root")
	}
	ctx := context.Background()
	parent := t.TempDir()
	dump := preGooseDump()

	cand, err := RebuildCandidate(ctx, parent, dump, mustMap(t, dump))
	if err != nil {
		t.Fatalf("RebuildCandidate: %v", err)
	}
	t.Cleanup(func() { _ = cand.Discard() })

	// Removing an entry needs write permission on its parent; drop it so the
	// candidate directory cannot be unlinked and RemoveAll fails.
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	if err := cand.Discard(); err == nil {
		_ = os.Chmod(parent, 0o755)
		t.Fatal("Discard succeeded despite an unwritable parent; expected a removal error")
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatalf("restore parent perms: %v", err)
	}

	if err := cand.Discard(); err != nil {
		t.Fatalf("retry Discard did not clear residue: %v", err)
	}
	if n := dirEntryCount(t, parent); n != 0 {
		t.Fatalf("directory residue survived retry: %d entries remain", n)
	}
}

// TestRebuildCandidateAttemptsAreIsolated proves two candidates built from one
// dump are independent: discarding the first removes only its directory and the
// second remains fully queryable. This is the structural property the
// per-attempt directory buys — no shared residue between attempts.
func TestRebuildCandidateAttemptsAreIsolated(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	dump := preGooseDump()

	first, err := RebuildCandidate(ctx, parent, dump, mustMap(t, dump))
	if err != nil {
		t.Fatalf("first RebuildCandidate: %v", err)
	}
	t.Cleanup(func() { _ = first.Discard() })
	second, err := RebuildCandidate(ctx, parent, dump, mustMap(t, dump))
	if err != nil {
		t.Fatalf("second RebuildCandidate: %v", err)
	}
	t.Cleanup(func() { _ = second.Discard() })

	if err := first.Discard(); err != nil {
		t.Fatalf("Discard first: %v", err)
	}
	// Discard is idempotent.
	if err := first.Discard(); err != nil {
		t.Fatalf("second Discard of the same candidate: %v", err)
	}

	export, err := second.Store().Export(ctx)
	if err != nil {
		t.Fatalf("second candidate unusable after first was discarded: %v", err)
	}
	if len(export.Issues) != 2 {
		t.Fatalf("second candidate lost data: want 2 issues, got %d", len(export.Issues))
	}
}
