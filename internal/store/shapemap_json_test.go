package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// aheadColumnMapping is the operator's correspondence for novelAheadDump: every
// recognizable column mapped, and the renamed `summary` reasoned onto `title` —
// the disposition the deterministic mapper cannot supply.
func aheadColumnMapping() ShapeMapping {
	return ShapeMapping{Columns: map[ColumnRef]Disposition{
		{Table: "issues", Column: "id"}:          MappedTo{Target: "issues.id"},
		{Table: "issues", Column: "summary"}:     MappedTo{Target: "issues.title"},
		{Table: "issues", Column: "description"}: MappedTo{Target: "issues.description"},
		{Table: "issues", Column: "prompt"}:      MappedTo{Target: "issues.prompt"},
		{Table: "issues", Column: "status"}:      MappedTo{Target: "issues.status"},
		{Table: "issues", Column: "priority"}:    MappedTo{Target: "issues.priority"},
		{Table: "issues", Column: "issue_type"}:  MappedTo{Target: "issues.issue_type"},
		{Table: "issues", Column: "assignee"}:    MappedTo{Target: "issues.assignee"},
		{Table: "issues", Column: "created_at"}:  MappedTo{Target: "issues.created_at"},
		{Table: "issues", Column: "updated_at"}:  MappedTo{Target: "issues.updated_at"},
		{Table: "issues", Column: "closed_at"}:   MappedTo{Target: "issues.closed_at"},

		{Table: "relations", Column: "src_id"}:     MappedTo{Target: "relations.src_id"},
		{Table: "relations", Column: "dst_id"}:     MappedTo{Target: "relations.dst_id"},
		{Table: "relations", Column: "type"}:       MappedTo{Target: "relations.type"},
		{Table: "relations", Column: "created_at"}: MappedTo{Target: "relations.created_at"},
		{Table: "relations", Column: "created_by"}: MappedTo{Target: "relations.created_by"},

		{Table: "comments", Column: "id"}:         MappedTo{Target: "comments.id"},
		{Table: "comments", Column: "issue_id"}:   MappedTo{Target: "comments.issue_id"},
		{Table: "comments", Column: "body"}:       MappedTo{Target: "comments.body"},
		{Table: "comments", Column: "created_at"}: MappedTo{Target: "comments.created_at"},
		{Table: "comments", Column: "created_by"}: MappedTo{Target: "comments.created_by"},

		{Table: "labels", Column: "issue_id"}:   MappedTo{Target: "labels.issue_id"},
		{Table: "labels", Column: "label"}:      MappedTo{Target: "labels.name"},
		{Table: "labels", Column: "created_at"}: MappedTo{Target: "labels.created_at"},
		{Table: "labels", Column: "created_by"}: MappedTo{Target: "labels.created_by"},

		{Table: "issue_events", Column: "id"}:         MappedTo{Target: "events.id"},
		{Table: "issue_events", Column: "issue_id"}:   MappedTo{Target: "events.issue_id"},
		{Table: "issue_events", Column: "action"}:     MappedTo{Target: "events.action"},
		{Table: "issue_events", Column: "reason"}:     MappedTo{Target: "events.reason"},
		{Table: "issue_events", Column: "assignee"}:   MappedTo{Target: "events.actor"},
		{Table: "issue_events", Column: "created_at"}: MappedTo{Target: "events.created_at"},

		{Table: "issue_event_changes", Column: "event_id"}:   MappedTo{Target: "event_changes.event_id"},
		{Table: "issue_event_changes", Column: "field"}:      MappedTo{Target: "event_changes.field"},
		{Table: "issue_event_changes", Column: "from_value"}: MappedTo{Target: "event_changes.from"},
		{Table: "issue_event_changes", Column: "to_value"}:   MappedTo{Target: "event_changes.to"},
	}}
}

// novelAheadDump models the genuine icqp incident shape: a workspace whose schema
// is AHEAD of the binary's vocabulary because an unmerged migration renamed a
// domain column. `issues.summary` is the renamed `title`. DeterministicMap must
// decline it; the operator's mapping recovers it.
func novelAheadDump() RawDump {
	const created = "2026-01-01T00:00:00Z"
	const closed = "2026-01-02T00:00:00Z"
	return RawDump{WorkspaceID: "ahead-ws", Tables: []RawTable{
		{Name: "issues",
			Columns: []string{"id", "summary", "description", "prompt", "status", "priority", "issue_type", "assignee", "created_at", "updated_at", "closed_at"},
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

// TestShapeMappingJSONRoundTrip locks the wire contract: a mapping survives
// Marshal→Unmarshal byte-for-byte in meaning. The artifact an operator authors
// is exactly the mapping the applier consumes — no field silently lost.
func TestShapeMappingJSONRoundTrip(t *testing.T) {
	original := aheadColumnMapping()
	original.Columns[ColumnRef{Table: "issues", Column: "obsolete"}] =
		Dropped{Provenance: DropUnexplained, Reason: "no target in baseline"}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ShapeMapping
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(original.Columns, got.Columns) {
		t.Fatalf("round-trip changed the mapping:\nwant %+v\ngot  %+v", original.Columns, got.Columns)
	}
}

// TestShapeMappingJSONStableOrder locks that the encoded artifact is sorted, so
// two encodings of the same mapping are byte-identical (diffable, reproducible).
func TestShapeMappingJSONStableOrder(t *testing.T) {
	a, err := json.Marshal(aheadColumnMapping())
	if err != nil {
		t.Fatalf("Marshal a: %v", err)
	}
	b, err := json.Marshal(aheadColumnMapping())
	if err != nil {
		t.Fatalf("Marshal b: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("encoding is not stable:\n%s\nvs\n%s", a, b)
	}
}

// TestShapeMappingJSONRejectsUnknownKind locks the decode boundary: a disposition
// kind with no sealed variant is rejected, not silently coerced into a drop or a
// map. This is what Validate cannot see — by decode time a bad kind would already
// be a nil interface.
func TestShapeMappingJSONRejectsUnknownKind(t *testing.T) {
	bad := `{"columns":[{"table":"issues","column":"id","kind":"transmute","to":"issues.id"}]}`
	var m ShapeMapping
	if err := json.Unmarshal([]byte(bad), &m); err == nil {
		t.Fatal("decode must reject an unknown disposition kind")
	} else if !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("error must name the unknown kind, got: %v", err)
	}
}

// TestShapeMappingJSONRejectsDuplicateColumn locks the other decode-only
// invariant: two entries for one column would silently collapse to whichever
// landed last, so the boundary rejects the ambiguity loudly.
func TestShapeMappingJSONRejectsDuplicateColumn(t *testing.T) {
	bad := `{"columns":[
		{"table":"issues","column":"id","kind":"map","to":"issues.id"},
		{"table":"issues","column":"id","kind":"drop","provenance":"unexplained"}
	]}`
	var m ShapeMapping
	if err := json.Unmarshal([]byte(bad), &m); err == nil {
		t.Fatal("decode must reject a duplicate column disposition")
	} else if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error must name the duplicate, got: %v", err)
	}
}

// TestDecodedMappingRecoversNovelAheadShape is the end-to-end proof the operator
// path turns on: a mapping authored as JSON, decoded through the wire form,
// recovers a workspace the deterministic mapper DECLINES — Doctor-clean, every
// issue conserved, the title values intact through the rename. This is the icqp
// deadend's recovery, exercised through the exact artifact the CLI consumes.
func TestDecodedMappingRecoversNovelAheadShape(t *testing.T) {
	ctx := context.Background()
	dump := novelAheadDump()

	if _, ok := DeterministicMap(dump); ok {
		t.Fatal("premise broken: DeterministicMap must DECLINE the novel ahead shape")
	}

	// Author as JSON, then consume only the decoded artifact — the CLI's path.
	data, err := json.Marshal(aheadColumnMapping())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var mapping ShapeMapping
	if err := json.Unmarshal(data, &mapping); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	outcome, err := Recover(ctx, canonicalUnder(t), dump, staticMapper(mapping), 1)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	recon, ok := outcome.(Reconciled)
	if !ok {
		t.Fatalf("want Reconciled, got %T: %+v", outcome, outcome)
	}
	t.Cleanup(func() { _ = recon.Candidate.Discard() })

	report, err := recon.Candidate.Store().Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	mustClean(t, report)

	export, err := recon.Candidate.Store().Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(export.Issues) != 2 {
		t.Fatalf("issue conservation broken: want 2, got %d", len(export.Issues))
	}
	titles := map[string]bool{}
	for _, is := range export.Issues {
		titles[is.Title] = true
	}
	if !titles["Open task"] || !titles["Done task"] {
		t.Fatalf("title values not preserved through the rename: got %+v", titles)
	}
}
