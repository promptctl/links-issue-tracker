package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// col is a FromColumn field source; const fields use Constant directly.
func col(c string, tr Transform) FieldSource { return FromColumn{Column: c, Transform: tr} }

// aheadColumnMapping is the operator's correspondence for novelAheadDump: every
// recognizable column mapped, and the renamed `summary` reasoned onto `title` —
// the disposition the deterministic mapper cannot supply. One Always emitter per
// table; timestamps and status carry their required transforms so the mapping is
// valid.
func aheadColumnMapping() ShapeMapping {
	return ShapeMapping{Tables: []TableMapping{
		{Table: "issues", Emitters: []Emitter{{Collection: collIssues, When: Always{}, Fields: map[string]FieldSource{
			"id":          col("id", TransformIdentity),
			"title":       col("summary", TransformIdentity),
			"description": col("description", TransformIdentity),
			"prompt":      col("prompt", TransformIdentity),
			"status":      col("status", TransformLegacyStatus),
			"priority":    col("priority", TransformIdentity),
			"issue_type":  col("issue_type", TransformIdentity),
			"assignee":    col("assignee", TransformIdentity),
			"created_at":  col("created_at", TransformTimestamp),
			"updated_at":  col("updated_at", TransformTimestamp),
			"closed_at":   col("closed_at", TransformTimestamp),
		}}}},
		{Table: "relations", Emitters: []Emitter{{Collection: collRelations, When: Always{}, Fields: map[string]FieldSource{
			"src_id":     col("src_id", TransformIdentity),
			"dst_id":     col("dst_id", TransformIdentity),
			"type":       col("type", TransformIdentity),
			"created_at": col("created_at", TransformTimestamp),
			"created_by": col("created_by", TransformIdentity),
		}}}},
		{Table: "comments", Emitters: []Emitter{{Collection: collComments, When: Always{}, Fields: map[string]FieldSource{
			"id":         col("id", TransformIdentity),
			"issue_id":   col("issue_id", TransformIdentity),
			"body":       col("body", TransformIdentity),
			"created_at": col("created_at", TransformTimestamp),
			"created_by": col("created_by", TransformIdentity),
		}}}},
		{Table: "labels", Emitters: []Emitter{{Collection: collLabels, When: Always{}, Fields: map[string]FieldSource{
			"issue_id":   col("issue_id", TransformIdentity),
			"name":       col("label", TransformIdentity),
			"created_at": col("created_at", TransformTimestamp),
			"created_by": col("created_by", TransformIdentity),
		}}}},
		{Table: "issue_events", Emitters: []Emitter{{Collection: collEvents, When: Always{}, Fields: map[string]FieldSource{
			"id":         col("id", TransformIdentity),
			"issue_id":   col("issue_id", TransformIdentity),
			"action":     col("action", TransformIdentity),
			"reason":     col("reason", TransformIdentity),
			"actor":      col("assignee", TransformIdentity),
			"created_at": col("created_at", TransformTimestamp),
		}}}},
		{Table: "issue_event_changes", Emitters: []Emitter{{Collection: collEventChanges, When: Always{}, Fields: map[string]FieldSource{
			"event_id": col("event_id", TransformIdentity),
			"field":    col("field", TransformIdentity),
			"from":     col("from_value", TransformIdentity),
			"to":       col("to_value", TransformIdentity),
		}}}},
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
// Marshal→Unmarshal→Marshal byte-for-byte. The artifact an operator authors is
// exactly the mapping the applier consumes — no field silently lost. Comparing
// re-encodings (rather than the in-memory structs) is the meaning-preserving
// equality, since Marshal is canonical/sorted.
func TestShapeMappingJSONRoundTrip(t *testing.T) {
	original := aheadColumnMapping()
	original.Tables[0].Drops = map[string]Dropped{
		"obsolete": {Provenance: DropUnexplained, Reason: "no target in baseline"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ShapeMapping
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	reencoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal(got): %v", err)
	}
	if string(data) != string(reencoded) {
		t.Fatalf("round-trip changed the mapping:\nwant %s\ngot  %s", data, reencoded)
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

// TestShapeMappingJSONStableOrderMultiEmitter locks the canonical wire form for
// the case the field-name-only sort key missed: a table carrying two emitters into
// the SAME collection that share field names but differ in source/condition. The
// encoding must be byte-identical regardless of the emitters' in-memory order, so
// the same mapping never has two encodings.
func TestShapeMappingJSONStableOrderMultiEmitter(t *testing.T) {
	emA := Emitter{Collection: collEventChanges, When: Always{}, Fields: map[string]FieldSource{
		"event_id": col("id", TransformIdentity),
		"field":    Constant{Value: "status"},
	}}
	emB := Emitter{Collection: collEventChanges, When: WhenChanged{FieldA: "field", FieldB: "event_id"}, Fields: map[string]FieldSource{
		"event_id": col("other", TransformIdentity),
		"field":    Constant{Value: "priority"},
	}}
	forward := ShapeMapping{Tables: []TableMapping{{Table: "t", Emitters: []Emitter{emA, emB}}}}
	reversed := ShapeMapping{Tables: []TableMapping{{Table: "t", Emitters: []Emitter{emB, emA}}}}

	a, err := json.Marshal(forward)
	if err != nil {
		t.Fatalf("Marshal forward: %v", err)
	}
	b, err := json.Marshal(reversed)
	if err != nil {
		t.Fatalf("Marshal reversed: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("two emitters into one collection encode order-dependently:\n%s\nvs\n%s", a, b)
	}
}

// TestShapeMappingJSONRejectsUnknownSource locks the decode boundary: a field
// source kind with no sealed variant is rejected, not silently coerced. This is
// what Validate cannot see — by decode time a bad kind would already have become
// a typed zero value.
func TestShapeMappingJSONRejectsUnknownSource(t *testing.T) {
	bad := `{"tables":[{"table":"issues","emitters":[{"collection":"issues","when":{"kind":"always"},"fields":[{"field":"id","source":"transmute","column":"id"}]}]}]}`
	var m ShapeMapping
	if err := json.Unmarshal([]byte(bad), &m); err == nil {
		t.Fatal("decode must reject an unknown field source")
	} else if !strings.Contains(err.Error(), "unknown source") {
		t.Fatalf("error must name the unknown source, got: %v", err)
	}
}

// TestShapeMappingJSONRejectsUnknownConditionKind locks the same boundary for the
// emit-condition discriminator.
func TestShapeMappingJSONRejectsUnknownConditionKind(t *testing.T) {
	bad := `{"tables":[{"table":"issues","emitters":[{"collection":"issues","when":{"kind":"sometimes"},"fields":[{"field":"id","source":"column","column":"id","transform":"identity"}]}]}]}`
	var m ShapeMapping
	if err := json.Unmarshal([]byte(bad), &m); err == nil {
		t.Fatal("decode must reject an unknown condition kind")
	} else if !strings.Contains(err.Error(), "unknown condition kind") {
		t.Fatalf("error must name the unknown condition kind, got: %v", err)
	}
}

// TestShapeMappingJSONRejectsDuplicateField locks a decode-only invariant: two
// entries for one field within an emitter would silently collapse to whichever
// landed last, so the boundary rejects the ambiguity loudly.
func TestShapeMappingJSONRejectsDuplicateField(t *testing.T) {
	bad := `{"tables":[{"table":"issues","emitters":[{"collection":"issues","when":{"kind":"always"},"fields":[
		{"field":"id","source":"column","column":"id","transform":"identity"},
		{"field":"id","source":"column","column":"other","transform":"identity"}
	]}]}]}`
	var m ShapeMapping
	if err := json.Unmarshal([]byte(bad), &m); err == nil {
		t.Fatal("decode must reject a duplicate field assignment")
	} else if !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("error must name the duplicate, got: %v", err)
	}
}

// TestShapeMappingJSONRejectsDuplicateTable locks that a table dispositioned twice
// is rejected — which fate wins would otherwise be ambiguous.
func TestShapeMappingJSONRejectsDuplicateTable(t *testing.T) {
	bad := `{"tables":[{"table":"issues","emitters":[]},{"table":"issues","emitters":[]}]}`
	var m ShapeMapping
	if err := json.Unmarshal([]byte(bad), &m); err == nil {
		t.Fatal("decode must reject a duplicate table disposition")
	} else if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error must name the duplicate, got: %v", err)
	}
}

// TestShapeMappingJSONRejectsUnknownField locks the trust boundary: an operator
// typo in a field name (here "prov" for "provenance") is rejected at decode rather
// than silently dropped, which would otherwise surface as a confusing downstream
// Validate error far from its cause.
func TestShapeMappingJSONRejectsUnknownField(t *testing.T) {
	bad := `{"tables":[{"table":"issues","drops":[{"column":"x","prov":"unexplained"}]}]}`
	var m ShapeMapping
	if err := json.Unmarshal([]byte(bad), &m); err == nil {
		t.Fatal("decode must reject an unknown field name")
	} else if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error must name the unknown field, got: %v", err)
	}
}

// TestShapeMappingJSONRejectsTrailingData locks the single-document property: a
// mapping file is one JSON object, so a valid object followed by more
// non-whitespace bytes is rejected rather than silently decoding only the first.
func TestShapeMappingJSONRejectsTrailingData(t *testing.T) {
	bad := `{"tables":[]}{"tables":[]}`

	var viaUnmarshal ShapeMapping
	if err := json.Unmarshal([]byte(bad), &viaUnmarshal); err == nil {
		t.Fatal("json.Unmarshal must reject trailing data after the mapping document")
	}

	// json.Unmarshal hands UnmarshalJSON only the first value's bytes, so the
	// method's own trailing-data guard is exercised by calling it directly with the
	// full multi-object blob — the path a delegated json.Decoder would take.
	var viaMethod ShapeMapping
	if err := viaMethod.UnmarshalJSON([]byte(bad)); err == nil {
		t.Fatal("UnmarshalJSON must reject trailing data when handed a multi-object blob")
	} else if !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("error must name the trailing data, got: %v", err)
	}

	// Trailing bytes that don't parse must preserve the underlying syntax error, so
	// the operator gets the location of the junk, not just "there was junk".
	garbage := `{"tables":[]}@@@nonsense`
	var viaGarbage ShapeMapping
	if err := viaGarbage.UnmarshalJSON([]byte(garbage)); err == nil {
		t.Fatal("UnmarshalJSON must reject unparseable trailing bytes")
	} else if !strings.Contains(err.Error(), "trailing data") || !strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("error must name trailing data AND preserve the underlying syntax error, got: %v", err)
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
