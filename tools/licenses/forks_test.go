package main

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

// forkOwnerPrefix is the only account allowed to own a module lit substitutes
// for an upstream one. Before links-licensing-c0ce.3 the dolt replace pointed at
// a personal GitHub account, which promptctl-deps-4aes had already flagged as a
// single point of failure for master's build.
const forkOwnerPrefix = "github.com/promptctl/"

// parseRootGoMod parses the repo's go.mod into the structure the two invariants
// below are stated over. [LAW:parse-dont-validate]
func parseRootGoMod(t *testing.T) *modfile.File {
	t.Helper()
	const path = "../../go.mod"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return f
}

// TestForkReplacementsAreOrgOwned pins the half of FORKS.md that CI can hold: a
// replace directive may substitute a remote module only with one this
// organization owns.
//
// Stated as a property over whatever replaces exist rather than as a list of the
// two that exist today, so adding a third org-owned fork needs no edit here and
// repointing any of them outside the org fails. [LAW:one-source-of-truth] — the
// set of forks lives in go.mod, and this file does not keep a second copy of it.
//
// The directory replacements (internal/vendor/dolthub-driver) are a different
// thing and correctly out of scope: modfile leaves New.Version empty for those,
// so the discriminator is in the parsed type rather than in a path-shape guess.
func TestForkReplacementsAreOrgOwned(t *testing.T) {
	for _, r := range parseRootGoMod(t).Replace {
		if r.New.Version == "" {
			continue // a directory replacement, not a fork
		}
		if !strings.HasPrefix(r.New.Path, forkOwnerPrefix) {
			t.Errorf("go.mod replaces %s with %s@%s, which is not under %s — "+
				"a fork lit builds from must be owned by the organization, not by "+
				"a personal account that can disappear; see FORKS.md",
				r.Old.Path, r.New.Path, r.New.Version, forkOwnerPrefix)
		}
	}
}

// TestForkedCoordinatesStayUpstream pins the other half: go.mod must keep
// requiring the UPSTREAM coordinate a fork diverged from, never the fork itself.
//
// That pairing is what makes the fork contract auditable. Because the require
// line still names upstream, `git diff <required-commit>..lit` inside the fork is
// a complete answer to "what did lit change?" — and an SBOM row keeps naming the
// coordinate the ecosystem knows. Rewrite a require to name the fork and both
// properties are gone, silently, with the build still green.
// [FRAMING:representation] the map keeps naming the territory it was drawn from.
func TestForkedCoordinatesStayUpstream(t *testing.T) {
	for _, r := range parseRootGoMod(t).Require {
		if strings.HasPrefix(r.Mod.Path, forkOwnerPrefix) {
			t.Errorf("go.mod requires %s directly; a fork must be reached through a "+
				"replace of the upstream coordinate, so that go.mod still records "+
				"which upstream commit lit diverged from (see FORKS.md)", r.Mod.Path)
		}
	}
}
