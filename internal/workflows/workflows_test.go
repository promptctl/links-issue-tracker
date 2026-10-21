package workflows

import (
	"os"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/environ"
)

func TestSetLookup(t *testing.T) {
	set := Set{Definitions: []Definition{{ID: "a"}, {ID: "b"}}}

	if def, ok := set.Lookup("b"); !ok || def.ID != "b" {
		t.Fatalf("Lookup(b) = %+v, %v, want the b definition", def, ok)
	}
	if _, ok := set.Lookup("missing"); ok {
		t.Fatalf("Lookup(missing) ok = true, want false")
	}
}

// testEnv is the test process's environment, read when it is called, so a
// test's t.Setenv reaches the load it runs.
func testEnv() environ.Env { return environ.Of(os.Environ()) }
