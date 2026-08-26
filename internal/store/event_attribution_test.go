package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// rawAttribution reads one event's attribution columns as the database actually
// holds them, because the distinction this feature depends on — SQL NULL versus
// the empty string — is exactly the one model.Attribution erases on the way out.
// A test that only read through Export could not tell a store that stamps
// nothing from one that stamps two empty strings into every row.
func rawAttribution(t *testing.T, ctx context.Context, st *Store, eventID string) (stream, workspace sql.NullString) {
	t.Helper()
	if err := st.db.QueryRowContext(ctx,
		`SELECT stream_id, workspace_id FROM issue_events WHERE id = ?`, eventID).Scan(&stream, &workspace); err != nil {
		t.Fatalf("read raw attribution for %s: %v", eventID, err)
	}
	return stream, workspace
}

// allEvents returns every event in the store, failing the test rather than
// returning an empty slice on error — an empty result and a failed read must not
// look alike to an assertion about "no event carries attribution".
func allEvents(t *testing.T, ctx context.Context, st *Store) []model.IssueEvent {
	t.Helper()
	events, err := st.ListAllEvents(ctx)
	if err != nil {
		t.Fatalf("ListAllEvents() error = %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events recorded — the fixture proves nothing about attribution")
	}
	return events
}

// exerciseEveryEventKind drives the three shapes of mutation that reach
// recordEvent — creation, a plain field update, and a named lifecycle
// transition — so an assertion over the resulting events covers all of them
// rather than whichever one a single-mutation fixture happened to pick.
func exerciseEveryEventKind(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	issue, err := st.CreateIssue(ctx, CreateIssueInput{
		Prefix: "test", Title: "Attributed work", Topic: "claims", IssueType: "task", Priority: 0,
	})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	newTitle := "Attributed work, retitled"
	if _, err := st.Apply(ctx, issue.ID, Change{
		Fields: UpdateIssueInput{Title: &newTitle}, Actor: "tester", Reason: "retitle",
	}); err != nil {
		t.Fatalf("Apply(field update) error = %v", err)
	}
	if _, err := st.Apply(ctx, issue.ID, Change{
		Action: model.Start{Assignee: "tester"}, Actor: "tester", Reason: "begin",
	}); err != nil {
		t.Fatalf("Apply(start) error = %v", err)
	}
}

// TestUnattributedStoreLeavesAttributionNull is the cold-start half of the
// feature: a store nobody attributed — every store opened for reading, and every
// store in existence before this feature — records events exactly as it always
// did. The columns hold SQL NULL, not empty strings, so the claim predicate that
// reads them later sees "this work has no producer" rather than "this work was
// produced by the empty checkout".
func TestUnattributedStoreLeavesAttributionNull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, migratedDoltDir(t), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	exerciseEveryEventKind(t, ctx, st)

	for _, event := range allEvents(t, ctx, st) {
		if event.Attribution.Present() {
			t.Errorf("event %s (%s) carries attribution %+v from a store nobody attributed",
				event.ID, event.Action, event.Attribution)
		}
		stream, workspace := rawAttribution(t, ctx, st, event.ID)
		if stream.Valid || workspace.Valid {
			t.Errorf("event %s stored stream=%#v workspace=%#v, want both SQL NULL",
				event.ID, stream, workspace)
		}
	}
}

// TestAttributedStoreStampsEveryEventKind is the write half: once the checkout's
// identity is known, every mutation that produces history carries the pair — not
// creation only, and not lifecycle transitions only. The stamp lives on the
// store rather than on each mutation's arguments, which is what makes "every"
// true here without ten call sites cooperating.
func TestAttributedStoreStampsEveryEventKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, migratedDoltDir(t), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	const token = "abc23456defgh"
	st.AttributeTo(token)
	exerciseEveryEventKind(t, ctx, st)

	want := model.NewAttribution(token, "test-workspace-id")
	for _, event := range allEvents(t, ctx, st) {
		if event.Attribution != want {
			t.Errorf("event %s (%s) attribution = %+v, want %+v",
				event.ID, event.Action, event.Attribution, want)
		}
	}
}

// TestAttributeToRejectsAHalfPair pins that an absent token yields no
// attribution at all rather than a workspace id with nothing to scope it. This
// is the shape a read-mode open in a never-mutated checkout produces, and it is
// the reason app.Open can hand the token over unconditionally instead of
// branching on whether one exists.
func TestAttributeToRejectsAHalfPair(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, migratedDoltDir(t), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	st.AttributeTo("")
	exerciseEveryEventKind(t, ctx, st)

	for _, event := range allEvents(t, ctx, st) {
		if event.Attribution.Present() {
			t.Errorf("event %s carries %+v, want nothing: a workspace id alone identifies no checkout",
				event.ID, event.Attribution)
		}
		if _, workspace := rawAttribution(t, ctx, st, event.ID); workspace.Valid {
			t.Errorf("event %s stored workspace_id %q with no stream_id — a half pair reached the database",
				event.ID, workspace.String)
		}
	}
}

// TestRestoreKeepsTheProducersAttribution pins that replaying a dump preserves
// who actually did the work. The restoring store is attributed to a DIFFERENT
// checkout than the events it is importing, so a re-stamping restore would
// rewrite every historical event into a claim for whoever ran the restore — and
// the assertion would catch it, which a same-token fixture could not.
func TestRestoreKeepsTheProducersAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source, err := Open(ctx, migratedDoltDir(t), "producer-workspace")
	if err != nil {
		t.Fatalf("Open(source) error = %v", err)
	}
	source.AttributeTo("producerstrm1")
	exerciseEveryEventKind(t, ctx, source)
	dump, err := source.Export(ctx)
	if err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("Close(source) error = %v", err)
	}

	target, err := Open(ctx, unrelatedDoltDir(t), "restorer-workspace")
	if err != nil {
		t.Fatalf("Open(target) error = %v", err)
	}
	defer target.Close()
	target.AttributeTo("restorerstrm")
	if err := target.ReplaceFromExport(ctx, dump); err != nil {
		t.Fatalf("ReplaceFromExport() error = %v", err)
	}

	want := model.NewAttribution("producerstrm1", "producer-workspace")
	for _, event := range allEvents(t, ctx, target) {
		if event.Attribution != want {
			t.Errorf("restored event %s attribution = %+v, want the producer's %+v",
				event.ID, event.Attribution, want)
		}
	}
}

// TestRestoringACorruptedExportWritesNoHalfPair drives the one route by which a
// half pair could reach issue_events despite nothing in this program being able
// to construct one: a sync file or backup is bytes lit did not write, and a
// hand-edited or truncated one can name a stream with no workspace. Restoring it
// must store nothing rather than a stream id scoping no workspace, because a
// row like that is invisible to the model (which reads it back as absent) while
// being plainly there to Doctor, dolt tooling, and any future migration.
//
// The corruption is injected into real exported JSON rather than into a
// hand-built fixture, so the bytes under test are the shape a real backup has.
func TestRestoringACorruptedExportWritesNoHalfPair(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source, err := Open(ctx, migratedDoltDir(t), "producer-workspace")
	if err != nil {
		t.Fatalf("Open(source) error = %v", err)
	}
	source.AttributeTo("producerstrm1")
	exerciseEveryEventKind(t, ctx, source)
	dump, err := source.Export(ctx)
	if err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("Close(source) error = %v", err)
	}

	corrupted := corruptEveryEventsAttribution(t, dump, map[string]any{"stream": "orphanstream"})

	target, err := Open(ctx, unrelatedDoltDir(t), "restorer-workspace")
	if err != nil {
		t.Fatalf("Open(target) error = %v", err)
	}
	defer target.Close()
	if err := target.ReplaceFromExport(ctx, corrupted); err != nil {
		t.Fatalf("ReplaceFromExport() error = %v", err)
	}

	for _, event := range allEvents(t, ctx, target) {
		if event.Attribution.Present() {
			t.Errorf("restored event %s carries %+v from a half-paired export",
				event.ID, event.Attribution)
		}
		stream, workspace := rawAttribution(t, ctx, target, event.ID)
		if stream.Valid || workspace.Valid {
			t.Errorf("restored event %s stored stream=%#v workspace=%#v; a half pair reached the table",
				event.ID, stream, workspace)
		}
	}
}

// TestRecoveredWorkspaceKeepsItsAttribution covers the recovery path, which
// reaches the attribution columns by an entirely separate route from
// Export/ReplaceFromExport: DumpRaw reads the raw tables, DeterministicMap
// proposes a column-to-field mapping, and Apply rebuilds the export. Three
// independent places have to agree on the field names — knownSourceColumns, the
// target registry, and assembleExport — and the other shapemap fixtures all run
// against stores that never called AttributeTo, so those columns are NULL there
// and a disagreement between the three would pass unnoticed.
//
// A recovered workspace that silently lost its attribution would look perfectly
// healthy: every ticket present, every event present, and every claim quietly
// gone, with nothing to point at the recovery as the cause.
func TestRecoveredWorkspaceKeepsItsAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src")
	// withStore opens with this id, and the pair the events carry is built from
	// it, so the assertion below names the store's real workspace rather than a
	// value this test chose.
	const ws = "test-workspace-id"
	const token = "recoverstrm7"

	withStore(t, ctx, src, func(st *Store) {
		st.AttributeTo(token)
		exerciseEveryEventKind(t, ctx, st)
	})

	dump, err := DumpRaw(ctx, src, ws)
	if err != nil {
		t.Fatalf("DumpRaw() error = %v", err)
	}
	mapping, ok := DeterministicMap(dump)
	if !ok {
		t.Fatal("DeterministicMap declined a dump carrying attribution — the new columns are outside its vocabulary")
	}
	if err := Validate(dump, mapping); err != nil {
		t.Fatalf("mapping for an attributed dump is not valid: %v", err)
	}
	export, err := Apply(dump, mapping)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if len(export.Events) == 0 {
		t.Fatal("recovered export holds no events — the fixture proves nothing")
	}
	want := model.NewAttribution(token, ws)
	for _, event := range export.Events {
		if event.Attribution != want {
			t.Errorf("recovered event %s attribution = %+v, want %+v",
				event.ID, event.Attribution, want)
		}
	}
}

// corruptEveryEventsAttribution re-serializes an export with each event's
// attribution replaced by raw JSON, then decodes it back through the ordinary
// export decoder — the same path a restore from a file takes. Going through the
// bytes is the point: the invariant under test belongs to decoding, so a test
// that mutated the struct directly would be testing a value this program cannot
// build and would prove nothing about a file it can actually be handed.
func corruptEveryEventsAttribution(t *testing.T, dump model.Export, attribution map[string]any) model.Export {
	t.Helper()
	encoded, err := json.Marshal(dump)
	if err != nil {
		t.Fatalf("marshal export error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("unmarshal export to generic form error = %v", err)
	}
	events, ok := raw["events"].([]any)
	if !ok || len(events) == 0 {
		t.Fatalf("export carries no events array to corrupt; got %T", raw["events"])
	}
	for _, event := range events {
		fields, ok := event.(map[string]any)
		if !ok {
			t.Fatalf("event is %T, not an object", event)
		}
		fields["attribution"] = attribution
	}
	recoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-marshal corrupted export error = %v", err)
	}
	var out model.Export
	if err := json.Unmarshal(recoded, &out); err != nil {
		t.Fatalf("decode corrupted export error = %v", err)
	}
	return out
}
