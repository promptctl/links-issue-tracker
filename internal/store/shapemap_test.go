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

	missingTitle := ShapeMapping{Tables: []TableMapping{{
		Table: "issues",
		Emitters: []Emitter{{Collection: collIssues, When: Always{}, Fields: map[string]FieldSource{
			"id": FromColumn{Column: "id", Transform: TransformIdentity},
		}}},
	}}}
	err := Validate(dump, missingTitle)
	if err == nil {
		t.Fatal("Validate accepted a mapping missing a source column")
	}
	if !strings.Contains(err.Error(), "title") || !strings.Contains(err.Error(), "unaccounted") {
		t.Fatalf("error must name the unaccounted column; got %v", err)
	}
}

func TestValidateRejectsStaleAndMalformedKeys(t *testing.T) {
	dump := RawDump{WorkspaceID: "w", Tables: []RawTable{
		{Name: "issues", Columns: []string{"id"}, Rows: nil},
	}}

	// issuesEmitter builds a one-emitter issues table from a field map, so each
	// case states only the fault it exercises.
	issuesEmitter := func(fields map[string]FieldSource) ShapeMapping {
		return ShapeMapping{Tables: []TableMapping{{
			Table:    "issues",
			Emitters: []Emitter{{Collection: collIssues, When: Always{}, Fields: fields}},
		}}}
	}
	cases := map[string]struct {
		mapping ShapeMapping
		want    string
	}{
		"source column the dump lacks": {
			mapping: issuesEmitter(map[string]FieldSource{
				"id":    FromColumn{Column: "id", Transform: TransformIdentity},
				"title": FromColumn{Column: "ghost", Transform: TransformIdentity},
			}),
			want: "does not have",
		},
		"unknown target field": {
			mapping: issuesEmitter(map[string]FieldSource{
				"nope": FromColumn{Column: "id", Transform: TransformIdentity},
			}),
			want: "unknown field",
		},
		"unknown collection": {
			mapping: ShapeMapping{Tables: []TableMapping{{
				Table:    "issues",
				Emitters: []Emitter{{Collection: "ghosts", When: Always{}, Fields: map[string]FieldSource{"id": FromColumn{Column: "id", Transform: TransformIdentity}}}},
			}}},
			want: "unknown collection",
		},
		"unknown drop provenance": {
			mapping: ShapeMapping{Tables: []TableMapping{{
				Table: "issues",
				Drops: map[string]Dropped{"id": {Provenance: "guessed", Reason: "x"}},
			}}},
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

// TestRejectsAmbiguousAlias proves a half-renamed workspace carrying BOTH a v1
// name and its pre-goose alias for one field is not silently collapsed. In the
// emitter model two sources cannot fill one field (the field map forbids it), so
// the ambiguity surfaces as an unaccounted-for column: the deterministic mapper
// declines rather than emit a mapping that drops one alias's data, and the public
// boundary rejects a hand-built mapping that leaves the other alias unaccounted.
func TestRejectsAmbiguousAlias(t *testing.T) {
	// An otherwise-complete issues table that carries BOTH the v1 name and its
	// pre-goose alias for the prompt field.
	dump := RawDump{WorkspaceID: "w", Tables: []RawTable{
		{Name: "issues",
			Columns: []string{"id", "title", "description", "status", "priority", "issue_type", "closed_at", "created_at", "updated_at", "prompt", "agent_prompt"},
			Rows:    [][]any{{"i1", "T", "", "open", int64(0), "task", nil, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", "a", "b"}}},
	}}

	if _, ok := DeterministicMap(dump); ok {
		t.Fatal("DeterministicMap must decline a dump that carries two aliases for one field")
	}

	// A mapping that maps only the v1 name leaves the pre-goose alias unaccounted —
	// rejected at the public Validate boundary. [LAW:single-enforcer]
	fields := map[string]FieldSource{
		"id":          FromColumn{Column: "id", Transform: TransformIdentity},
		"title":       FromColumn{Column: "title", Transform: TransformIdentity},
		"description": FromColumn{Column: "description", Transform: TransformIdentity},
		"status":      FromColumn{Column: "status", Transform: TransformLegacyStatus},
		"priority":    FromColumn{Column: "priority", Transform: TransformIdentity},
		"issue_type":  FromColumn{Column: "issue_type", Transform: TransformIdentity},
		"closed_at":   FromColumn{Column: "closed_at", Transform: TransformTimestamp},
		"created_at":  FromColumn{Column: "created_at", Transform: TransformTimestamp},
		"updated_at":  FromColumn{Column: "updated_at", Transform: TransformTimestamp},
		"prompt":      FromColumn{Column: "agent_prompt", Transform: TransformIdentity},
	}
	leavesAliasUnaccounted := ShapeMapping{Tables: []TableMapping{{
		Table:    "issues",
		Emitters: []Emitter{{Collection: collIssues, When: Always{}, Fields: fields}},
	}}}
	err := Validate(dump, leavesAliasUnaccounted)
	if err == nil || !strings.Contains(err.Error(), "unaccounted") {
		t.Fatalf("Validate must reject a mapping that leaves the prompt alias unaccounted; got %v", err)
	}
}

// TestDeterministicMapDeclinesThinNonIssuesTable proves the recognizable-shape
// gate covers every domain table, not just issues: a comments table missing
// body/id would otherwise let the restore path persist a fabricated empty
// comment (ReplaceFromExport does raw inserts, bypassing ImportComment's
// required-field checks).
func TestDeterministicMapDeclinesThinNonIssuesTable(t *testing.T) {
	// A complete issues table (so issues is not the reason to decline) plus a
	// comments table carrying only issue_id.
	dump := RawDump{WorkspaceID: "w", Tables: []RawTable{
		{Name: "issues",
			Columns: []string{"id", "title", "description", "status", "priority", "issue_type", "closed_at", "created_at", "updated_at"},
			Rows:    [][]any{{"i1", "T", "", "open", int64(0), "task", nil, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"}}},
		{Name: "comments", Columns: []string{"issue_id"}, Rows: [][]any{{"i1"}}},
	}}
	if _, ok := DeterministicMap(dump); ok {
		t.Fatal("DeterministicMap must decline a comments table missing required target fields")
	}

	// The same partial shape must be rejected at the public Validate boundary —
	// an LLM-supplied mapping that omits required targets cannot slip past it.
	// [LAW:single-enforcer]
	partial := ShapeMapping{Tables: []TableMapping{
		fullIssuesMapping(dump.Tables[0]),
		{Table: "comments", Emitters: []Emitter{{Collection: collComments, When: Always{}, Fields: map[string]FieldSource{
			"issue_id": FromColumn{Column: "issue_id", Transform: TransformIdentity},
		}}}},
	}}
	err := Validate(dump, partial)
	if err == nil || !strings.Contains(err.Error(), "does not cover required field") {
		t.Fatalf("Validate must reject a mapping that omits required targets; got %v", err)
	}
}

// fullIssuesMapping builds a complete one-emitter issues TableMapping for a dump
// table by routing through the deterministic mapper — so tests that need a valid
// issues table beside an intentionally-faulty other table state only the fault.
func fullIssuesMapping(table RawTable) TableMapping {
	tm, ok := simpleEmitter(table, knownSourceColumns["issues"])
	if !ok {
		panic("fullIssuesMapping: issues table not recognized")
	}
	return tm
}

// TestDeterministicMapDeclinesThinRequiredTargets proves the gate requires every
// non-optional target of a collection (derived from the registry), so no thin
// table — whatever the collection — reaches Apply and fabricates empty required
// fields. Optional fields (e.g. issues.topic, events.action) are not required.
func TestDeterministicMapDeclinesThinRequiredTargets(t *testing.T) {
	cases := map[string]RawTable{
		// issues covering the historical-shape columns but missing id/title:
		// Apply would build an issue with empty id/title.
		"issues missing id/title": {
			Name:    "issues",
			Columns: []string{"description", "status", "priority", "issue_type", "closed_at", "created_at", "updated_at"},
			Rows:    [][]any{{"", "open", int64(0), "task", nil, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"}},
		},
		// issue_events missing reason/actor: Apply would build an event with
		// empty reason/actor.
		"events missing reason/actor": {
			Name:    "issue_events",
			Columns: []string{"id", "issue_id", "action", "created_at"},
			Rows:    [][]any{{"e1", "i1", "done", "2026-01-01T00:00:00Z"}},
		},
	}
	for name, table := range cases {
		t.Run(name, func(t *testing.T) {
			dump := RawDump{WorkspaceID: "w", Tables: []RawTable{table}}
			if _, ok := DeterministicMap(dump); ok {
				t.Fatalf("DeterministicMap must decline %s", name)
			}
		})
	}
}

// TestValidateRejectsRowArityMismatch proves a malformed dump with a row whose
// cell count differs from the column count is rejected at the boundary, rather
// than panicking on a positional index deep in Apply. The dump may be a
// serialized artifact, so this is real external input.
func TestValidateRejectsRowArityMismatch(t *testing.T) {
	// A complete issues shape (so coverage passes and the arity check is what
	// fires) whose single row is one cell short of the column count.
	cols := []string{"id", "title", "description", "status", "priority", "issue_type", "closed_at", "created_at", "updated_at"}
	dump := RawDump{WorkspaceID: "w", Tables: []RawTable{
		{Name: "issues", Columns: cols, Rows: [][]any{{"i1", "T", "", "open", int64(0), "task", nil, "2026-01-01T00:00:00Z"}}}, // 8 cells, 9 columns
	}}
	mapping := ShapeMapping{Tables: []TableMapping{fullIssuesMapping(dump.Tables[0])}}
	err := Validate(dump, mapping)
	if err == nil || !strings.Contains(err.Error(), "cells, want") {
		t.Fatalf("Validate must reject a row/column arity mismatch; got %v", err)
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
		"ALTER TABLE `relations` DROP COLUMN IF EXISTS `old_col`;\n" +
		"ALTER TABLE issues DROP COLUMN a, DROP COLUMN b;"
	refs := parseDroppedColumns(sqlText)
	got := map[ColumnRef]bool{}
	for _, r := range refs {
		got[r] = true
	}
	// Every dropped column is captured, including both drops in the
	// multi-DROP-COLUMN statement.
	for _, want := range []ColumnRef{{"issues", "legacy_field"}, {"relations", "old_col"}, {"issues", "a"}, {"issues", "b"}} {
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
		if _, err := st.StartIssue(ctx, StartIssueInput{IssueID: solo.ID, Assignee: "claude", Reason: "begin", CreatedBy: "claude"}); err != nil {
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
// value — [LAW:no-silent-failure], the conservation guarantee a recovery tool
// must hold.
func TestApplyRejectsCorruptTimestamp(t *testing.T) {
	dump := RawDump{WorkspaceID: "w", Tables: []RawTable{
		{Name: "issues",
			Columns: []string{"id", "title", "description", "status", "priority", "issue_type", "closed_at", "created_at", "updated_at"},
			Rows: [][]any{
				{"i1", "T", "", "open", int64(0), "task", nil, "not-a-timestamp", "2026-01-01T00:00:00Z"},
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
