package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// forkOwnerPrefix is the only account allowed to own a module lit substitutes
// for an upstream one. Before links-licensing-c0ce.3 the dolt replace pointed at
// a personal GitHub account, which promptctl-deps-4aes had already flagged as a
// single point of failure for master's build.
const forkOwnerPrefix = "github.com/promptctl/"

// forkLedgerPath is FORKS.md, the written fork contract. go.mod points at it
// instead of summarizing itself, so the ledger necessarily quotes go.mod back —
// module paths, both pins, and the short commits its runnable `git diff` snippet
// depends on.
//
// [LAW:one-source-of-truth] go.mod is the authority and the ledger is a derived
// copy. What makes that legal rather than the drift this whole ticket reacted to
// is the pair of tests below: they synchronize the copy explicitly, in both
// directions, on every `go test ./...`. The stale go.mod comment this replaced
// went wrong precisely because nothing did that.
const forkLedgerPath = "../../FORKS.md"

var (
	// A Go version or pseudo-version, spelled the same way in go.mod and in the
	// ledger's tables.
	versionToken = regexp.MustCompile(`\bv[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.\-]+)?`)
	// The 12-hex short commit a pseudo-version ends with. The ledger also quotes
	// it bare, in the invariant table and in the `git diff <base>..lit` snippet.
	shaToken = regexp.MustCompile(`\b[0-9a-f]{12}\b`)
)

// parseRootGoMod parses the repo's go.mod into the structure every invariant
// below is stated over. [LAW:parse-dont-validate]
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

func readForkLedger(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(forkLedgerPath)
	if err != nil {
		t.Fatalf("read %s: %v", forkLedgerPath, err)
	}
	return string(data)
}

// moduleReplaces returns the replace directives that substitute one module
// coordinate for another — the forks. modfile leaves New.Version empty for a
// directory replacement (internal/vendor/dolthub-driver), so the discriminator
// is already in the parsed type and needs no path-shape guess.
func moduleReplaces(f *modfile.File) []*modfile.Replace {
	var out []*modfile.Replace
	for _, r := range f.Replace {
		if r.New.Version != "" {
			out = append(out, r)
		}
	}
	return out
}

func requiredVersions(f *modfile.File) map[string]string {
	out := make(map[string]string, len(f.Require))
	for _, r := range f.Require {
		out[r.Mod.Path] = r.Mod.Version
	}
	return out
}

// TestForkReplacementsAreOrgOwned pins the first half of the contract: a replace
// directive may substitute a remote module only with one this organization owns.
//
// Stated as a property over whatever replaces exist rather than as a list of the
// two that exist today, so adding a third org-owned fork needs no edit here and
// repointing any of them outside the org fails. [LAW:one-source-of-truth] — the
// set of forks lives in go.mod, and this file keeps no second copy of it.
func TestForkReplacementsAreOrgOwned(t *testing.T) {
	for _, r := range moduleReplaces(parseRootGoMod(t)) {
		if !strings.HasPrefix(r.New.Path, forkOwnerPrefix) {
			t.Errorf("go.mod replaces %s with %s@%s, which is not under %s — "+
				"a fork lit builds from must be owned by the organization, not by "+
				"a personal account that can disappear; see FORKS.md",
				r.Old.Path, r.New.Path, r.New.Version, forkOwnerPrefix)
		}
	}
}

// TestForkedCoordinatesStayUpstream pins the second half: go.mod must keep
// requiring the UPSTREAM coordinate a fork diverged from, never the fork itself.
//
// That pairing is what makes the contract auditable. Because the require line
// still names upstream, `git diff <required-commit>..lit` inside the fork is a
// complete answer to "what did lit change?" — and an SBOM row keeps naming the
// coordinate the ecosystem knows. Adopt the fork as the coordinate and both
// properties are gone, silently, with the build still green.
//
// The property is "no require names a replace target," which is true by
// construction. An earlier version of this test asked whether a require was
// under forkOwnerPrefix — a stronger theorem that is not true: a sibling
// org-owned library that forks nothing is a legitimate dependency, and that
// version would have failed it with advice that made no sense.
// [FRAMING:representation] the map keeps naming the territory it was drawn from.
func TestForkedCoordinatesStayUpstream(t *testing.T) {
	f := parseRootGoMod(t)
	substitutes := make(map[string]string, len(f.Replace))
	for _, r := range f.Replace {
		substitutes[r.New.Path] = r.Old.Path
	}
	for _, req := range f.Require {
		if upstream, ok := substitutes[req.Mod.Path]; ok {
			t.Errorf("go.mod requires %s directly, but that module is the "+
				"replacement for %s; a fork must be reached only through the "+
				"replace, so go.mod still records which upstream commit lit "+
				"diverged from (see FORKS.md)", req.Mod.Path, upstream)
		}
	}
}

// TestForkLedgerQuotesEveryCurrentPin walks go.mod → FORKS.md: every coordinate
// the build actually uses must appear in the ledger.
//
// This is the check that catches a pin moving without the prose moving with it,
// including the case no human edits at all: go-mysql-server is required
// indirectly, so `go mod tidy` can raise its require line on its own the moment
// a rebased dolt wants a newer one. The versionless replace keeps substituting
// fork source built from the OLD commit, so go.mod would name an upstream commit
// the fork is not based on and the ledger's `git diff` command would start
// returning upstream churn mixed with lit's patches. Nothing else notices; this
// does. [LAW:no-silent-failure]
func TestForkLedgerQuotesEveryCurrentPin(t *testing.T) {
	f := parseRootGoMod(t)
	ledger := readForkLedger(t)
	required := requiredVersions(f)

	for _, r := range moduleReplaces(f) {
		quoted := map[string]string{
			"the upstream module path": r.Old.Path,
			"the fork pin it builds":   r.New.Version,
		}
		if v := required[r.Old.Path]; v != "" {
			quoted["the upstream version go.mod requires"] = v
			if sha := shaToken.FindString(v); sha != "" {
				quoted["the short commit its git-diff snippet needs"] = sha
			}
		}
		for what, s := range quoted {
			if !strings.Contains(ledger, s) {
				t.Errorf("go.mod substitutes %s, but FORKS.md never mentions %s (%q).\n"+
					"If you just moved a pin, update FORKS.md's two tables and the "+
					"git-diff snippet. If you did NOT touch go.mod, `go mod tidy` "+
					"raised an indirect require on its own — the fork branch is no "+
					"longer based on the commit go.mod names, and it has to be "+
					"rebased onto the new one before this passes.",
					r.Old.Path, what, s)
			}
		}
	}
}

// TestForkLedgerQuotesNothingStale walks FORKS.md → go.mod: every version and
// commit the ledger states as fact must be one go.mod actually contains.
//
// Scope is deliberate. Only markdown table rows and fenced code blocks are read,
// because those are where the ledger makes claims about what lit builds and
// hands the reader commands to run. Prose is exempt: it cites
// golang-lru@v2.0.7 to explain why a replace cannot remove a coordinate, and
// removing that coordinate is this epic's goal — a check that read prose would
// fail at the exact moment the epic succeeded.
func TestForkLedgerQuotesNothingStale(t *testing.T) {
	f := parseRootGoMod(t)

	stated := map[string]bool{}
	record := func(version string) {
		stated[version] = true
		for _, sha := range shaToken.FindAllString(version, -1) {
			stated[sha] = true
		}
	}
	for _, r := range f.Require {
		record(r.Mod.Version)
	}
	for _, r := range moduleReplaces(f) {
		record(r.New.Version)
	}

	for i, line := range claimLines(readForkLedger(t)) {
		for _, tok := range append(
			versionToken.FindAllString(line, -1),
			shaToken.FindAllString(line, -1)...,
		) {
			if !stated[tok] {
				t.Errorf("FORKS.md:%d states %q, which go.mod does not contain — "+
					"the ledger is quoting a pin the build no longer uses:\n  %s",
					i+1, tok, strings.TrimSpace(line))
			}
		}
	}
}

// TestVendoredDriverMirrorsForkReplaces synchronizes the third home of the fork
// pins. The vendored driver's go.mod mirrors the root's fork replaces so that,
// resolved standalone, it builds against the forks and cannot re-record a
// coordinate the forks removed (golang-lru arrived back that way once, as an
// indirect require through the upstream go-mysql-server go.mod).
//
// [LAW:one-source-of-truth] the root go.mod stays the authority; the mirror is
// a derived copy, and this test is what makes a derived copy legal — the same
// standing the two ledger tests above give FORKS.md. Both directions are
// checked: a fork the driver requires must be mirrored, and a mirror must match
// the root pin exactly. Driver-local replaces for coordinates the root does not
// fork (flatbuffers) are out of scope — those are the driver's own business.
func TestVendoredDriverMirrorsForkReplaces(t *testing.T) {
	rootForks := make(map[string]module.Version)
	for _, r := range moduleReplaces(parseRootGoMod(t)) {
		rootForks[r.Old.Path] = r.New
	}

	const driverPath = "../../internal/vendor/dolthub-driver/go.mod"
	data, err := os.ReadFile(driverPath)
	if err != nil {
		t.Fatalf("read %s: %v", driverPath, err)
	}
	driver, err := modfile.Parse(driverPath, data, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", driverPath, err)
	}

	mirrored := make(map[string]module.Version)
	for _, r := range driver.Replace {
		if want, forked := rootForks[r.Old.Path]; forked {
			mirrored[r.Old.Path] = r.New
			if r.New != want {
				t.Errorf("%s replaces %s with %s@%s, but the root go.mod pins the "+
					"fork at %s@%s — the mirror has drifted; move it with the root "+
					"pin (see FORKS.md)",
					driverPath, r.Old.Path, r.New.Path, r.New.Version,
					want.Path, want.Version)
			}
		}
	}
	for _, req := range driver.Require {
		if _, forked := rootForks[req.Mod.Path]; !forked {
			continue
		}
		if _, ok := mirrored[req.Mod.Path]; !ok {
			t.Errorf("%s requires %s, which the root go.mod builds from a fork, "+
				"but carries no mirroring replace — resolved standalone it would "+
				"build upstream source and can re-record coordinates the fork "+
				"removed (see FORKS.md)", driverPath, req.Mod.Path)
		}
	}
}

// claimLines returns the ledger's lines that assert something checkable — table
// rows and fenced code — with every other line blanked so indices still line up
// with the file for error messages.
func claimLines(ledger string) []string {
	lines := strings.Split(ledger, "\n")
	out := make([]string, len(lines))
	fenced := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced || strings.HasPrefix(strings.TrimSpace(line), "|") {
			out[i] = line
		}
	}
	return out
}
