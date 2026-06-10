package model

import "testing"

// TestParseRelationType pins the sealed vocabulary and the trust-boundary
// contract: trimmed legal names parse to their constant, everything else is
// rejected with the documented message. [LAW:behavior-not-structure]
func TestParseRelationType(t *testing.T) {
	legal := map[string]RelationType{
		"blocks":         RelBlocks,
		"parent-child":   RelParentChild,
		"related-to":     RelRelatedTo,
		"  blocks  ":     RelBlocks,
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
	wantMsg := "relation type must be blocks, parent-child, or related-to"
	for _, in := range []string{"", "Blocks", "blocked", "parent_child", "depends-on", "blocks,parent-child"} {
		if _, err := ParseRelationType(in); err == nil || err.Error() != wantMsg {
			t.Errorf("ParseRelationType(%q) error = %v, want %q", in, err, wantMsg)
		}
	}
}

// TestRelationTypeStoreEndpoints pins the orientation contract: blocks maps
// the human "<blocker> <blocked>" order to the store's dependent->dependency
// encoding and is its own inverse; other types pass through.
func TestRelationTypeStoreEndpoints(t *testing.T) {
	if src, dst := RelBlocks.StoreEndpoints("blocker", "blocked"); src != "blocked" || dst != "blocker" {
		t.Errorf("blocks StoreEndpoints = (%s, %s), want (blocked, blocker)", src, dst)
	}
	// Involution: applying the mapping twice restores the original order.
	a, b := RelBlocks.StoreEndpoints(RelBlocks.StoreEndpoints("x", "y"))
	if a != "x" || b != "y" {
		t.Errorf("blocks StoreEndpoints twice = (%s, %s), want (x, y)", a, b)
	}
	for _, rt := range []RelationType{RelParentChild, RelRelatedTo} {
		if src, dst := rt.StoreEndpoints("from", "to"); src != "from" || dst != "to" {
			t.Errorf("%s StoreEndpoints = (%s, %s), want (from, to)", rt, src, dst)
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
	for _, rt := range []RelationType{RelBlocks, RelParentChild} {
		if src, dst := rt.CanonicalEndpoints("zzz", "aaa"); src != "zzz" || dst != "aaa" {
			t.Errorf("%s CanonicalEndpoints = (%s, %s), want (zzz, aaa)", rt, src, dst)
		}
	}
}
