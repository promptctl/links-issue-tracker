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
// coordinate for another — the forks — as opposed to the directory replacement
// (internal/vendor/dolthub-driver), which these fork-contract tests do not
// govern.
//
// It asks parseReplacement rather than testing New.Version itself. That
// function owns the module-versus-directory question for this package, and it
// treats an empty version as necessary but NOT sufficient evidence of a
// directory; a local copy here reading only the version would be a second,
// weaker spelling of one rule, free to disagree with the one the artifacts are
// rendered from. [LAW:single-enforcer]
//
// The error arm is a guard, not a gate: modfile.Parse rejects a versioned
// directory target and a version-less module target one line earlier, so
// parseReplacement is unlikely to be the first to complain. It is checked
// rather than discarded because a value quietly dropped on the floor is how the
// two spellings would drift apart again.
func moduleReplaces(t *testing.T, f *modfile.File) []*modfile.Replace {
	t.Helper()
	var out []*modfile.Replace
	for _, r := range f.Replace {
		replacement, err := parseReplacement(r.Old.Path, r.New.Path, r.New.Version)
		if err != nil {
			t.Fatalf("go.mod replaces %s with a target parseReplacement refuses (%s => %s %s): %v",
				r.Old.Path, r.Old.Path, r.New.Path, r.New.Version, err)
		}
		if replacement.Kind == ReplacedByFork {
			out = append(out, r)
		}
	}
	// Every caller states a property over this slice — org ownership, ledger
	// coverage, the vendored mirror — and every one of those properties is
	// vacuously true of an empty slice. Since the classification moved from
	// `New.Version != ""` to parseReplacement's path comparison, a fork
	// misfiled as ReplacedByVersion would empty this set and turn three green
	// tests into three tests of nothing. Fail here instead, once, where the
	// reason is legible. [LAW:verifiable-goals]
	if len(out) == 0 {
		t.Fatalf("go.mod declares %d replace directive(s) but parseReplacement classified none of them as a fork; "+
			"the fork-contract tests below would all pass without examining anything. If lit genuinely stopped "+
			"forking, delete them — they have no subject. Otherwise a fork is being misclassified.", len(f.Replace))
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
	for _, r := range moduleReplaces(t, parseRootGoMod(t)) {
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

	for _, r := range moduleReplaces(t, f) {
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
	for _, r := range moduleReplaces(t, f) {
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

// collapseWhitespace folds every run of whitespace into a single space, so a
// sentence in the ledger can be matched against a constant no matter where
// markdown's line wrapping happened to break it. The ledger's quotation of the
// heading below spans two lines today and would span one after any reflow;
// without this, a reflow alone would fail the check that follows.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TestForkLedgerQuotesTheGraphSectionTitle binds the one sentence in FORKS.md
// that quotes the graph audit's own heading to the constant that prints it.
//
// The ledger read "MODULES WHOSE SOURCE COMES FROM A DIFFERENT COORDINATE" for
// exactly as long as sectionReplaced did. Renaming the constant — a version pin
// substitutes the SAME coordinate, so the old title was false for a shape the
// section now holds — left the ledger quoting a heading no run of the tool
// emits, and nothing failed.
//
// [FRAMING:representation] a quotation is a copy, and a copy a human must
// remember to redraw is one that has already begun to lie. This gives that copy
// the same standing the two pin tests above give the ledger's tables.
func TestForkLedgerQuotesTheGraphSectionTitle(t *testing.T) {
	if ledger := collapseWhitespace(readForkLedger(t)); !strings.Contains(ledger, sectionReplaced) {
		t.Errorf("FORKS.md does not quote the graph audit's replaced-modules heading %q — "+
			"the ledger tells a reader which section of `licenses -graph` prints this fact, "+
			"so renaming the heading means re-quoting it there in the same change", sectionReplaced)
	}
}

// TestForkLedgerNamesEverySubstitution backs a promise the SHIPPED artifacts
// make. LICENSE-REPORT.md's legend and each substituted section of
// THIRD_PARTY_LICENSES send a recipient to FORKS.md for what the substitution
// changes and why — for every substituted row, whatever its kind.
//
// TestForkLedgerQuotesEveryCurrentPin above governs forks alone, so without
// this a directory or version-pin replacement could be added to go.mod,
// disclosed correctly in all three artifacts, and point every reader at a
// document that never mentions it — a dangling pointer inside a compliance
// artifact, which is the same defect class as an undisclosed substitution.
func TestForkLedgerNamesEverySubstitution(t *testing.T) {
	f := parseRootGoMod(t)
	ledger := readForkLedger(t)
	for _, r := range f.Replace {
		replacement, err := parseReplacement(r.Old.Path, r.New.Path, r.New.Version)
		if err != nil {
			t.Fatalf("go.mod replaces %s with a target parseReplacement refuses (%s => %s %s): %v",
				r.Old.Path, r.Old.Path, r.New.Path, r.New.Version, err)
		}
		if replacement.Kind == NotReplaced {
			continue
		}
		if !strings.Contains(ledger, r.New.Path) {
			t.Errorf("go.mod builds %s from %s, and the shipped artifacts tell every reader of that "+
				"row to see FORKS.md — but FORKS.md never names %s. Document the substitution there, "+
				"or the report and the bundle are pointing at a page that does not answer them.",
				r.Old.Path, r.New.Path, r.New.Path)
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
	for _, r := range moduleReplaces(t, parseRootGoMod(t)) {
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

	sumPath := strings.TrimSuffix(driverPath, ".mod") + ".sum"
	sumData, err := os.ReadFile(sumPath)
	if err != nil {
		t.Fatalf("read %s: %v", sumPath, err)
	}

	// The driver's own replaces, predating the fork mirrors and owned by it
	// alone. Everything else must be a mirror of a current root fork — a
	// replace for a coordinate the root no longer forks is a leftover wherever
	// it points, org account or personal.
	driverLocalReplaces := map[string]bool{
		"github.com/google/flatbuffers": true,
	}

	mirrored := make(map[string]module.Version)
	for _, r := range driver.Replace {
		want, forked := rootForks[r.Old.Path]
		if !forked {
			if !driverLocalReplaces[r.Old.Path] {
				t.Errorf("%s replaces %s with %s@%s, but the root go.mod does "+
					"not fork that coordinate and it is not a known driver-local "+
					"replace — delete the stale mirror, or add it to "+
					"driverLocalReplaces if the driver genuinely owns it "+
					"(see FORKS.md)",
					driverPath, r.Old.Path, r.New.Path, r.New.Version)
			}
			continue
		}
		mirrored[r.Old.Path] = r.New
		if r.New != want {
			t.Errorf("%s replaces %s with %s@%s, but the root go.mod pins the "+
				"fork at %s@%s — the mirror has drifted; move it with the root "+
				"pin (see FORKS.md)",
				driverPath, r.Old.Path, r.New.Path, r.New.Version,
				want.Path, want.Version)
		}
		// The pin's second derived home is the driver's lockfile: a mirror
		// moved without `go mod tidy` in the driver leaves go.sum without the
		// new version's hashes, and only a standalone build would notice. Both
		// lines matter — the module zip's and its go.mod's — because a partial
		// re-tidy can write one without the other.
		for kind, suffix := range map[string]string{"module": " ", "go.mod": "/go.mod "} {
			if !strings.Contains(string(sumData), r.New.Path+" "+r.New.Version+suffix) {
				t.Errorf("%s has no %s hash entry for %s@%s — the mirror in "+
					"go.mod moved without re-tidying the driver; run "+
					"`go mod tidy` in internal/vendor/dolthub-driver",
					sumPath, kind, r.New.Path, r.New.Version)
			}
		}
	}
	forkTargets := make(map[string]string, len(rootForks))
	for upstream, target := range rootForks {
		forkTargets[target.Path] = upstream
	}
	for _, req := range driver.Require {
		// The same pairing TestForkedCoordinatesStayUpstream pins for the root:
		// require the upstream coordinate, reach the fork only through the
		// replace — a direct require of the fork target severs the diff-against-
		// upstream answer here exactly as it would there.
		if upstream, isTarget := forkTargets[req.Mod.Path]; isTarget {
			t.Errorf("%s requires %s directly, but that module is the fork "+
				"replacement for %s; require the upstream coordinate and mirror "+
				"the root's replace instead (see FORKS.md)",
				driverPath, req.Mod.Path, upstream)
		}
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
