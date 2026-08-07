package workflows

import "testing"

func TestSetLookup(t *testing.T) {
	set := Set{Definitions: []Definition{{ID: "a"}, {ID: "b"}}}

	if def, ok := set.Lookup("b"); !ok || def.ID != "b" {
		t.Fatalf("Lookup(b) = %+v, %v, want the b definition", def, ok)
	}
	if _, ok := set.Lookup("missing"); ok {
		t.Fatalf("Lookup(missing) ok = true, want false")
	}
}
