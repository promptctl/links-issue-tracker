package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// --- Validate: the single totality enforcer (acceptance #1) ---

func TestValidateRejectsIncompleteMapping(t *testing.T) {
	dump := RawDump{WorkspaceID: "w", Tables: []RawTable{
		{Name: "issues", Columns: []string{"id", "title"}, Rows: [][]any{{"i1", "T"}}},
	}}

	missingTitle := ShapeMapping{Columns: map[ColumnRef]Disposition{
		{Table: "issues", Column: "id"}: to("issues.id"),
	}}
	err := Validate(dump, missingTitle)
	if err == nil {
		t.Fatal("Validate accepted a mapping missing a source column")
	}
	if !strings.Contains(err.Error(), "issues.title") {
		t.Fatalf("error must name the unaccounted column; got %v", err)
	}
}

func TestValidateRejectsStaleAndMalformedKeys(t *testing.T) {
	dump := RawDump{WorkspaceID: "w", Tables: []RawTable{
		{Name: "issues", Columns: []string{"id"}, Rows: nil},
	}}

	cases := map[string]struct {
		mapping ShapeMapping
		want    string
	}{
		"stale key": {
			mapping: ShapeMapping{Columns: map[ColumnRef]Disposition{
				{Table: "issues", Column: "id"}:    to("issues.id"),
				{Table: "issues", Column: "ghost"}: to("issues.title"),
			}},
			want: "does not have",
		},
		"unknown target": {
			mapping: ShapeMapping{Columns: map[ColumnRef]Disposition{
				{Table: "issues", Column: "id"}: MappedTo{Target: "issues.nope"},
			}},
			want: "unknown target",
		},
		"nil disposition": {
			mapping: ShapeMapping{Columns: map[ColumnRef]Disposition{
				{Table: "issues", Column: "id"}: nil,
			}},
			want: "neither MappedTo nor Dropped",
		},
		"unknown drop provenance": {
			mapping: ShapeMapping{Columns: map[ColumnRef]Disposition{
				{Table: "issues", Column: "id"}: Dropped{Provenance: "guessed", Reason: "x"},
			}},
			want: "unknown drop provenance",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Validate(dump, tc.mapping)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestRejectsDuplicateTargets proves a table whose two columns claim the same
// domain field is rejected rather than letting one silently overwrite the other
// — and that the deterministic mapper declines such a dump (a half-renamed
// workspace carrying both prompt and agent_prompt) instead of emitting a lossy
// mapping.
func TestRejectsDuplicateTargets(t *testing.T) {
	dump := RawDump{WorkspaceID: "w", Tables: []RawTable{
		{Name: "issues", Columns: []string{"prompt", "agent_prompt"}, Rows: [][]any{{"a", "b"}}},
	}}

	ambiguous := ShapeMapping{Columns: map[ColumnRef]Disposition{
		{Table: "issues", Column: "prompt"}:       to("issues.prompt"),
		{Table: "issues", Column: "agent_prompt"}: to("issues.prompt"),
	}}
	err := Validate(dump, ambiguous)
	if err == nil || !strings.Contains(err.Error(), "both map to") {
		t.Fatalf("Validate must reject two columns mapping to one field; got %v", err)
	}

	if _, ok := DeterministicMap(dump); ok {
		t.Fatal("DeterministicMap must decline a dump that maps two columns to one field")
	}
}

// --- Drop provenance distinguishable from migration history (acceptance #2) ---

func TestClassifyDropDistinguishesProvenance(t *testing.T) {
	if prov, reason := classifyDrop("goose_db_version", "version_id"); prov != DropIntended || reason == "" {
		t.Fatalf("bookkeeping column: got (%q,%q), want intended with a reason", prov, reason)
	}
	if prov, _ := classifyDrop("issues", "a_column_no_migration_ever_made"); prov != DropUnexplained {
		t.Fatalf("unknown column: got %q, want unexplained", prov)
	}
}

func TestParseDroppedColumnsReadsMigrationHistory(t *testing.T) {
	sqlText := "ALTER TABLE issues DROP COLUMN legacy_field;\n" +
		"ALTER TABLE `relations` DROP COLUMN IF EXISTS `old_col`;"
	refs := parseDroppedColumns(sqlText)
	got := map[ColumnRef]bool{}
	for _, r := range refs {
		got[r] = true
	}
	for _, want := range []ColumnRef{{"issues", "legacy_field"}, {"relations", "old_col"}} {
		if !got[want] {
			t.Fatalf("parseDroppedColumns missed %v; got %v", want, refs)
		}
	}
}

// TestGooseUpSectionExcludesDownDrops proves intended drops are read from the
// forward (Up) direction only. A migration that ADDs a column in Up DROPs it in
// Down; reading Down would misclassify a kept column as an intended drop and
// could justify discarding live data.
func TestGooseUpSectionExcludesDownDrops(t *testing.T) {
	addInUpDropInDown := "-- +goose Up\n" +
		"ALTER TABLE issues ADD COLUMN foo TEXT;\n" +
		"-- +goose Down\n" +
		"ALTER TABLE issues DROP COLUMN foo;\n"
	if refs := parseDroppedColumns(gooseUpSection(addInUpDropInDown)); len(refs) != 0 {
		t.Fatalf("a Down-only DROP COLUMN must not count as an intended drop; got %v", refs)
	}

	realDrop := "-- +goose Up\n" +
		"ALTER TABLE issues DROP COLUMN legacy;\n" +
		"-- +goose Down\n" +
		"ALTER TABLE issues ADD COLUMN legacy TEXT;\n"
	refs := parseDroppedColumns(gooseUpSection(realDrop))
	if len(refs) != 1 || refs[0] != (ColumnRef{Table: "issues", Column: "legacy"}) {
		t.Fatalf("an Up DROP COLUMN must be captured; got %v", refs)
	}
}

// --- The two known shapes produce valid total mappings (acceptance #3) ---

func mustClean(t *testing.T, report HealthReport) {
	t.Helper()
	if report.IntegrityCheck != "ok" || len(report.Errors) != 0 {
		t.Fatalf("rebuilt workspace is not clean: integrity=%q errors=%v", report.IntegrityCheck, report.Errors)
	}
}

// TestDeterministicMapCleanAhead dumps a real v1 workspace, maps it, and proves
// the produced Export rebuilds into a clean workspace with every issue
// conserved — exercising the actual dumper and the typed write path.
func TestDeterministicMapCleanAhead(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src")
	const ws = "test-workspace-id"

	wantIDs := map[string]bool{}
	withStore(t, ctx, src, func(st *Store) {
		epic, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Epic", IssueType: "epic", Topic: "recovery", Prefix: "links"})
		if err != nil {
			t.Fatalf("create epic: %v", err)
		}
		child, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Child task", IssueType: "task", Topic: "recovery", Prefix: "links", ParentID: epic.ID})
		if err != nil {
			t.Fatalf("create child: %v", err)
		}
		solo, err := st.CreateIssue(ctx, CreateIssueInput{Title: "Solo task", IssueType: "task", Topic: "recovery", Prefix: "links"})
		if err != nil {
			t.Fatalf("create solo: %v", err)
		}
		if _, err := st.AddComment(ctx, AddCommentInput{IssueID: child.ID, Body: "a note", CreatedBy: "claude"}); err != nil {
			t.Fatalf("add comment: %v", err)
		}
		if _, err := st.AddLabel(ctx, AddLabelInput{IssueID: child.ID, Name: "backend", CreatedBy: "claude"}); err != nil {
			t.Fatalf("add label: %v", err)
		}
		if _, err := st.TransitionIssue(ctx, TransitionIssueInput{IssueID: solo.ID, Action: "start", Reason: "begin", CreatedBy: "claude", Assignee: "claude"}); err != nil {
			t.Fatalf("start solo: %v", err)
		}
		for _, id := range []string{epic.ID, child.ID, solo.ID} {
			wantIDs[id] = true
		}
	})

	dump, err := DumpRaw(ctx, src, ws)
	if err != nil {
		t.Fatalf("DumpRaw: %v", err)
	}
	mapping, ok := DeterministicMap(dump)
	if !ok {
		t.Fatal("DeterministicMap declined a clean v1 dump")
	}
	if err := Validate(dump, mapping); err != nil {
		t.Fatalf("clean-ahead mapping is not valid: %v", err)
	}
	export, err := Apply(dump, mapping)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	gotIDs := map[string]bool{}
	var epicID string
	for _, issue := range export.Issues {
		gotIDs[issue.ID] = true
		if issue.IssueType == "epic" {
			epicID = issue.ID
		}
	}
	for id := range wantIDs {
		if !gotIDs[id] {
			t.Fatalf("issue %q lost across map+apply; got %v", id, gotIDs)
		}
	}

	dst := filepath.Join(t.TempDir(), "dst")
	withStore(t, ctx, dst, func(st *Store) {
		if err := st.ReplaceFromExport(ctx, export); err != nil {
			t.Fatalf("ReplaceFromExport: %v", err)
		}
		report, err := st.Doctor(ctx)
		if err != nil {
			t.Fatalf("Doctor: %v", err)
		}
		mustClean(t, report)
		// The epic's NULL status must survive the round trip as NULL, not be
		// coerced to a leaf status.
		epic, err := st.GetIssue(ctx, epicID)
		if err != nil {
			t.Fatalf("get rebuilt epic: %v", err)
		}
		if epic.StatusValue() != "" {
			t.Fatalf("rebuilt epic status = %q, want empty (NULL)", epic.StatusValue())
		}
	})
}

// TestDeterministicMapPreGoose maps a hand-built pre-goose shape — `prompt`
// instead of `agent_prompt`, `issue_events.assignee` instead of `actor`, legacy
// status vocabulary, no goose_db_version, no topic/item_rank — and proves it
// rebuilds clean with the aliased values and canonicalized statuses intact.
func TestDeterministicMapPreGoose(t *testing.T) {
	ctx := context.Background()
	const created = "2026-01-01T00:00:00Z"
	const closed = "2026-01-02T00:00:00Z"

	dump := RawDump{WorkspaceID: "legacy-ws", Tables: []RawTable{
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

	mapping, ok := DeterministicMap(dump)
	if !ok {
		t.Fatal("DeterministicMap declined a known pre-goose dump")
	}
	if err := Validate(dump, mapping); err != nil {
		t.Fatalf("pre-goose mapping is not valid: %v", err)
	}
	export, err := Apply(dump, mapping)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(export.Issues) != 2 {
		t.Fatalf("want 2 issues, got %d", len(export.Issues))
	}
	if len(export.Events) != 1 || len(export.Events[0].Changes) != 1 {
		t.Fatalf("event nesting lost: events=%+v", export.Events)
	}
	if export.Events[0].Actor != "claude" {
		t.Fatalf("issue_events.assignee did not map to events.actor: %q", export.Events[0].Actor)
	}

	dst := filepath.Join(t.TempDir(), "dst")
	withStore(t, ctx, dst, func(st *Store) {
		if err := st.ReplaceFromExport(ctx, export); err != nil {
			t.Fatalf("ReplaceFromExport: %v", err)
		}
		report, err := st.Doctor(ctx)
		if err != nil {
			t.Fatalf("Doctor: %v", err)
		}
		mustClean(t, report)

		i1, err := st.GetIssue(ctx, "i1")
		if err != nil {
			t.Fatalf("get i1: %v", err)
		}
		if i1.Prompt != "the historical prompt" {
			t.Fatalf("prompt alias lost: %q", i1.Prompt)
		}
		if i1.StatusValue() != "open" {
			t.Fatalf("legacy status 'todo' not canonicalized: %q", i1.StatusValue())
		}
		if i1.Topic != "misc" {
			t.Fatalf("absent topic not defaulted by write path: %q", i1.Topic)
		}
		i2, err := st.GetIssue(ctx, "i2")
		if err != nil {
			t.Fatalf("get i2: %v", err)
		}
		if i2.StatusValue() != "closed" {
			t.Fatalf("legacy status 'done' not canonicalized: %q", i2.StatusValue())
		}
	})
}

// --- The NULL-vs-"" distinction is load-bearing and preserved ---

func TestApplyTransformPreservesNull(t *testing.T) {
	// An epic's NULL status must never be coerced to a leaf value, and a NULL
	// timestamp must never become a zero time, by any transform.
	for _, tf := range []Transform{TransformIdentity, TransformLegacyStatus, TransformTimestamp} {
		got, err := applyTransform(tf, nil)
		if err != nil {
			t.Fatalf("%s(nil) error = %v", tf, err)
		}
		if got != nil {
			t.Fatalf("%s(nil) = %v, want nil (NULL preserved)", tf, got)
		}
	}
	if got, _ := applyTransform(TransformLegacyStatus, "todo"); got != "open" {
		t.Fatalf("legacy 'todo' = %v, want 'open'", got)
	}
}

// TestApplyRejectsCorruptTimestamp proves a non-NULL timestamp that does not
// parse fails loudly with source context rather than vanishing into a zero
// value — [LAW:no-silent-fallbacks], the conservation guarantee a recovery tool
// must hold.
func TestApplyRejectsCorruptTimestamp(t *testing.T) {
	dump := RawDump{WorkspaceID: "w", Tables: []RawTable{
		{Name: "issues",
			Columns: []string{"id", "title", "description", "status", "priority", "issue_type", "created_at", "updated_at"},
			Rows: [][]any{
				{"i1", "T", "", "open", int64(0), "task", "not-a-timestamp", "2026-01-01T00:00:00Z"},
			}},
	}}
	mapping, ok := DeterministicMap(dump)
	if !ok {
		t.Fatal("DeterministicMap declined a known shape")
	}
	_, err := Apply(dump, mapping)
	if err == nil {
		t.Fatal("Apply accepted a corrupt timestamp instead of surfacing it")
	}
	for _, want := range []string{"issues", "created_at", "not-a-timestamp"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must carry source context %q; got %v", want, err)
		}
	}
}
