package model

import (
	"encoding/json"
	"testing"
)

// TestParseRelationType pins the sealed vocabulary and the trust-boundary
// contract: trimmed legal names parse to their constant, everything else is
// rejected with the documented message. [LAW:behavior-not-structure]
func TestParseRelationType(t *testing.T) {
	legal := map[string]RelationType{
		"blocked-by":     RelBlockedBy,
		"parent-child":   RelParentChild,
		"related-to":     RelRelatedTo,
		"  blocked-by  ": RelBlockedBy,
		"\trelated-to\n": RelRelatedTo,
	}
	for in, want := range legal {
		got, err := ParseRelationType(in)
		if err != nil {
			t.Errorf("ParseRelationType(%q) error = %v, want %q", in, err, want)
			continue
		}
		if got != want {
			t.Errorf("ParseRelationType(%q) = %q, want %q", in, got, want)
		}
	}
	wantMsg := "relation type must be blocked-by, parent-child, or related-to"
	for _, in := range []string{"", "blocks", "Blocked-By", "blocked_by", "parent_child", "depends-on", "blocked-by,parent-child"} {
		if _, err := ParseRelationType(in); err == nil || err.Error() != wantMsg {
			t.Errorf("ParseRelationType(%q) error = %v, want %q", in, err, wantMsg)
		}
	}
}

// TestRelationTypeFromStored pins the legacy reader: the pre-v8 spelling of
// the blocked-by edge reads as today's, and every other spelling — legal or
// not — passes through for the write boundary to judge.
func TestRelationTypeFromStored(t *testing.T) {
	for in, want := range map[string]RelationType{
		"blocks":       RelBlockedBy,
		"blocked-by":   RelBlockedBy,
		"parent-child": RelParentChild,
		"related-to":   RelRelatedTo,
		"depends-on":   RelationType("depends-on"),
	} {
		if got := RelationTypeFromStored(in); got != want {
			t.Errorf("RelationTypeFromStored(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExportDecodeRenamesLegacyBlocksEdges pins that an export written before
// version 3 reads its "blocks" rows as blocked-by edges with the same
// endpoints, so a restore of an old backup or sync file keeps every edge's
// meaning, while a version-3 export is read as written: "blocks" is not a
// version-3 spelling, and renaming it there would hide a corrupt file.
func TestExportDecodeRenamesLegacyBlocksEdges(t *testing.T) {
	const relations = `"relations":[{"src_id":"waiter","dst_id":"blocker","type":"blocks"},{"src_id":"child","dst_id":"epic","type":"parent-child"}]`
	for _, tc := range []struct {
		version  int
		wantType RelationType
	}{
		{version: 1, wantType: RelBlockedBy},
		{version: 2, wantType: RelBlockedBy},
		{version: ExportVersion, wantType: RelationType("blocks")},
	} {
		var export Export
		doc := `{"version":` + string(rune('0'+tc.version)) + `,` + relations + `}`
		if err := json.Unmarshal([]byte(doc), &export); err != nil {
			t.Fatalf("version %d: Unmarshal: %v", tc.version, err)
		}
		want := []Relation{
			{SrcID: "waiter", DstID: "blocker", Type: tc.wantType},
			{SrcID: "child", DstID: "epic", Type: RelParentChild},
		}
		if len(export.Relations) != len(want) {
			t.Fatalf("version %d: relations = %+v, want %+v", tc.version, export.Relations, want)
		}
		for i := range want {
			if export.Relations[i] != want[i] {
				t.Errorf("version %d: relation %d = %+v, want %+v", tc.version, i, export.Relations[i], want[i])
			}
		}
	}
}

// TestRelationTypeCanonicalEndpoints pins the storage normalization contract:
// related-to is undirected so its endpoint pair has one stored representation;
// directed types keep their orientation.
func TestRelationTypeCanonicalEndpoints(t *testing.T) {
	if src, dst := RelRelatedTo.CanonicalEndpoints("zzz", "aaa"); src != "aaa" || dst != "zzz" {
		t.Errorf("related-to CanonicalEndpoints(zzz, aaa) = (%s, %s), want (aaa, zzz)", src, dst)
	}
	if src, dst := RelRelatedTo.CanonicalEndpoints("aaa", "zzz"); src != "aaa" || dst != "zzz" {
		t.Errorf("related-to CanonicalEndpoints(aaa, zzz) = (%s, %s), want (aaa, zzz)", src, dst)
	}
	for _, rt := range []RelationType{RelBlockedBy, RelParentChild} {
		if src, dst := rt.CanonicalEndpoints("zzz", "aaa"); src != "zzz" || dst != "aaa" {
			t.Errorf("%s CanonicalEndpoints = (%s, %s), want (zzz, aaa)", rt, src, dst)
		}
	}
}
