package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// graphAuditEnv gates the tests that resolve and scan the WHOLE module graph.
const graphAuditEnv = "LIT_LICENSE_GRAPH_AUDIT"

// requireWholeGraph skips a test unless the whole-graph audit is explicitly
// requested, naming the command that runs it.
//
// These are this ticket's acceptance checks and they are also, by a wide
// margin, the most expensive thing in this repository's test suite: resolving
// the graph means `go mod download all`, which fetches every module the build
// does not need — 3.4 GB and several minutes against a cold cache — and then
// walking 588 module trees. CI's build-and-test job budgets under five minutes
// TOTAL for the whole gate, and setup-go saves GOMODCACHE into a 10 GB
// repo-wide cache keyed on go.sum, so leaving these ungated would blow the time
// budget on every pull request and evict every other cache entry as a bonus.
//
// The logic these cover is not going unwatched. Everything that can be decided
// without the real graph — the accept/reject tables for what gets scanned and
// what gets recorded, the section routing, the eliding, the root-grant
// ambiguity rule — runs on fixtures on every PR. What is gated is precisely
// the part that needs 588 real modules to mean anything.
// [LAW:verifiable-goals] the check still exists and still runs; it runs where
// its cost is affordable.
func requireWholeGraph(t *testing.T) {
	t.Helper()
	if os.Getenv(graphAuditEnv) == "" {
		t.Skipf("whole-graph audit not requested; run with %s=1 go test ./tools/licenses/ (downloads the full module graph)", graphAuditEnv)
	}
}

// writeFixture creates dir/name with content, making parent directories as
// needed, so a test can state a module's on-disk shape as a path list.
func writeFixture(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestScanLicenseTextsAcceptReject is the accept/reject table for what the
// graph scan will even look at. Every shape below was taken from this
// repository's actual module graph, not invented: the rejected names are files
// that really appear (Oracle's SDK models a license-management API in ~90 Go
// files, CycloneDX ships license fixtures as JSON and XML, zap has a
// checklicense.sh), and the accepted ones include the two conventions that
// matter — a module that NAMES the file and a module that names the FOLDER.
//
// The freetype shape is the reason the location route exists at all: its root
// LICENSE is a pointer document naming licenses/gpl.txt, so a scan keyed only
// on filenames sees no GPL anywhere in the module. [LAW:types-are-the-program]
func TestScanLicenseTextsAcceptReject(t *testing.T) {
	root := t.TempDir()

	// Accepted by the name route.
	writeFixture(t, root, "LICENSE", "MIT")
	writeFixture(t, root, "COPYING.LESSER", "LGPL")
	writeFixture(t, root, "Sun-LICENSE", "whatever")
	writeFixture(t, root, "testdata/deep/nested/COPYING", "GPL")
	// Accepted by the location route: names that announce nothing.
	writeFixture(t, root, "licenses/gpl.txt", "GPL")
	writeFixture(t, root, "licenses/ftl.txt", "FTL")
	// Rejected: nothing about the name or the location suggests a license.
	writeFixture(t, root, "main.go", "package main")
	writeFixture(t, root, "README.md", "docs")
	writeFixture(t, root, "internal/notice.txt", "not a grant")

	got, err := scanLicenseTexts(root)
	if err != nil {
		t.Fatalf("scanLicenseTexts: %v", err)
	}
	found := make(map[string]bool, len(got))
	for _, p := range got {
		found[p] = true
	}

	for _, want := range []string{
		"LICENSE",
		"COPYING.LESSER",
		"Sun-LICENSE",
		filepath.Join("testdata", "deep", "nested", "COPYING"),
		filepath.Join("licenses", "gpl.txt"),
		filepath.Join("licenses", "ftl.txt"),
	} {
		if !found[want] {
			t.Errorf("scan missed %s; found %v", want, got)
		}
	}
	for _, reject := range []string{"main.go", "README.md", filepath.Join("internal", "notice.txt")} {
		if found[reject] {
			t.Errorf("scan picked up %s, which is not license-shaped by name or location", reject)
		}
	}
}

// TestScanLicenseTextsIsSorted pins the determinism the audit depends on:
// filepath.WalkDir's order is lexical per directory but the result is sorted
// explicitly, so two runs over one module produce byte-identical reports.
func TestScanLicenseTextsIsSorted(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"zzz-LICENSE", "LICENSE", "licenses/mmm.txt", "aaa-COPYING"} {
		writeFixture(t, root, n, "x")
	}
	got, err := scanLicenseTexts(root)
	if err != nil {
		t.Fatalf("scanLicenseTexts: %v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("results not sorted at %d: %v", i, got)
		}
	}
}

// TestIsLicenseTextAcceptReject is the accept/reject table for the second
// filter — the one that runs after classification and decides whether a
// candidate is recorded at all.
//
// The rule has to hold two things at once. An unmatched LICENSE file is the
// most important row in the whole audit (nobody can say what it permits) and
// must survive; an unmatched .go file whose name merely contains "license" is
// noise and must not. Anything the classifier DID match is a license text
// whatever it is called, which is what keeps licenses/gpl.txt — a name that
// announces nothing — in the report.
func TestIsLicenseTextAcceptReject(t *testing.T) {
	cases := []struct {
		name    string
		relPath string
		license string
		want    bool
		why     string
	}{
		{"matched license at root", "LICENSE", "MIT", true, "classified — always a license text"},
		{"matched license with an unhelpful name", "licenses/gpl.txt", "GPL-2.0", true, "the classifier, not the filename, decides"},
		{"matched license in a .go file", "embedded_license.go", "Apache-2.0", true, "a real grant can be embedded; the classifier matched it"},
		{"unmatched grant at root", "LICENSE", unclassifiedLicense, true, "an unclassifiable grant is the worst row, never dropped"},
		{"unmatched grant, qualified name", "SQLITE-LICENSE", unclassifiedLicense, true, "no extension — plausibly prose"},
		{"unmatched grant, text extension", "LICENSE-link.txt", unclassifiedLicense, true, "text, plausibly prose"},
		{"unmatched Go source", "license_type.go", unclassifiedLicense, false, "machine content, not a grant"},
		{"unmatched shell script", "checklicense.sh", unclassifiedLicense, false, "machine content"},
		{"unmatched CI config", ".licenserc.yml", unclassifiedLicense, false, "machine content"},
		{"unmatched JSON fixture", "valid-license-id.json", unclassifiedLicense, false, "machine content"},
		{"unmatched XML fixture", "valid-license-id.xml", unclassifiedLicense, false, "machine content"},
		{"oversize binary database", "licenses/licenses.db", oversizeLicense, false, "machine content, and never read"},
		{"oversize concatenated bundle", "Godeps/LICENSES", oversizeLicense, true, "unread, but plausibly a real bundle — must be reported as skipped"},
		{"uppercase machine extension", "License_Type.GO", unclassifiedLicense, false, "extension matching is case-insensitive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLicenseText(tc.relPath, tc.license); got != tc.want {
				t.Errorf("isLicenseText(%q, %q) = %v, want %v — %s", tc.relPath, tc.license, got, tc.want, tc.why)
			}
		})
	}
}

// TestIsRootGrantSeparatesCoordinateFromTree pins the audit's central
// distinction. A coordinate scanner reports a module's root grant; a
// file-walking scanner reports everything else. Getting this backwards would
// file a vendored test corpus as a real obligation and a real obligation as
// noise.
func TestIsRootGrantSeparatesCoordinateFromTree(t *testing.T) {
	for _, tc := range []struct {
		relPath string
		want    bool
	}{
		{"LICENSE", true},
		{"COPYING.LESSER", true},
		{"licenses/gpl.txt", false},
		{"testdata/nsz.repo.hu/libc-test/src/math/crlibm/COPYING", false},
	} {
		if got := (LicenseHit{RelPath: tc.relPath}).IsRootGrant(); got != tc.want {
			t.Errorf("IsRootGrant(%q) = %v, want %v", tc.relPath, got, tc.want)
		}
	}
}

// TestElidePerModuleCountsWhatItHides pins the property that makes eliding
// safe: a reader is never told less than the truth about how many rows exist,
// only shown fewer of them. A module under the cap is untouched.
func TestElidePerModuleCountsWhatItHides(t *testing.T) {
	rows := []graphRow{
		{Module: "small", Path: "LICENSE", License: "GPL-2.0"},
		{Module: "big", Path: "a.txt", License: "AGPL-3.0"},
		{Module: "big", Path: "b.txt", License: "GPL-1.0"},
		{Module: "big", Path: "c.txt", License: "GPL-2.0"},
		{Module: "big", Path: "d.txt", License: "GPL-3.0"},
		{Module: "big", Path: "e.txt", License: "OSL-3.0"},
		{Module: "other", Path: "LICENSE", License: "CC-BY-4.0"},
	}
	got := elidePerModule(rows, 3)

	if len(got) != 1+3+1+1 {
		t.Fatalf("got %d rows, want 6 (small + 3 of big + elision + other): %+v", len(got), got)
	}
	if got[0].Module != "small" || got[0].Path != "LICENSE" {
		t.Errorf("a module under the cap must pass through untouched, got %+v", got[0])
	}
	elision := got[4]
	if !strings.Contains(elision.Path, "2 more") {
		t.Errorf("elision row must state how many were held back, got %q", elision.Path)
	}
	if elision.Module != "big" {
		t.Errorf("elision row must name the module it summarizes, got %q", elision.Module)
	}
	if last := got[len(got)-1]; last.Module != "other" {
		t.Errorf("rows after an elided module must survive, got %+v", last)
	}
}

// TestPartitionGraphRoutesEachFinding pins where each kind of hit lands, which
// is what the whole report means. A permitted license is reported nowhere; an
// unclassifiable one goes to its own section whether or not it sits at the
// root, because "we cannot say what this permits" is a different problem from
// "this is copyleft".
func TestPartitionGraphRoutesEachFinding(t *testing.T) {
	policy := &Policy{AllowedLicenses: []string{"MIT", "Apache-2.0"}}
	entries := []GraphEntry{
		{Module: Module{Path: "clean", Version: "v1"}, Hits: []LicenseHit{{RelPath: "LICENSE", License: "MIT"}}},
		{Module: Module{Path: "copyleft-root", Version: "v1"}, Hits: []LicenseHit{{RelPath: "LICENSE", License: "GPL-2.0"}}},
		{Module: Module{Path: "corpus", Version: "v1"}, Hits: []LicenseHit{{RelPath: "testdata/COPYING", License: "GPL-2.0"}}},
		{Module: Module{Path: "murky", Version: "v1"}, Hits: []LicenseHit{{RelPath: "LICENSE", License: unclassifiedLicense}}},
		{Module: Module{Path: "bare", Version: "v1"}},
		{Module: Module{Path: "forked", Version: "v1", Replacement: Replacement{Kind: ReplacedByFork, Path: "other/fork", Version: "v2"}}, Hits: []LicenseHit{{RelPath: "LICENSE", License: "MIT"}}},
	}

	byTitle := make(map[string][]graphRow)
	for _, s := range partitionGraph(entries, policy.Filter()) {
		byTitle[s.Title] = s.Rows
	}

	wantOnly := map[string]string{
		sectionModuleGrants: "copyleft-root",
		sectionNestedTexts:  "corpus",
		sectionUnclassified: "murky",
		sectionNoLicense:    "bare",
		sectionReplaced:     "forked",
	}
	for title, wantModule := range wantOnly {
		rows := byTitle[title]
		if len(rows) != 1 {
			t.Errorf("%s: got %d rows, want exactly 1 (%s): %+v", title, len(rows), wantModule, rows)
			continue
		}
		if rows[0].Module != wantModule {
			t.Errorf("%s: got module %q, want %q", title, rows[0].Module, wantModule)
		}
	}

	// The permitted module must appear in no findings section at all — a
	// report that lists compliant modules alongside violations teaches its
	// readers to skim.
	for title, rows := range byTitle {
		if title == sectionReplaced {
			continue
		}
		for _, r := range rows {
			if r.Module == "clean" {
				t.Errorf("%s lists a policy-permitted module", title)
			}
		}
	}
}

// TestModuleExceptionsReachOnlyTheRootGrant pins that a policy exception
// excuses the file a human actually read and nothing else.
//
// A policy exception names one license string a human verified against one
// file — the module's ROOT grant (as policy.json once did for fslock's
// LGPL-with-static-linking-exception, before links-licensing-c0ce.4 removed
// that dependency). If the exception also covered a copyleft file buried in
// the same module's testdata, the report would drop it on the strength of a
// human having read a different file. An allowlisted license is different:
// permissive is permissive at any depth. [LAW:no-silent-failure]
//
// The excepted license here is LGPL-3.0 rather than the classifier's Unknown
// sentinel, which is what this test used while kch42/buzhash's unclassifiable
// WTFPL variant rode an exception. That is no longer a shape a policy can
// express — see TestSentinelLicensesHaveNoPathThroughAnyFilter.
func TestModuleExceptionsReachOnlyTheRootGrant(t *testing.T) {
	policy := &Policy{
		AllowedLicenses:  []string{"MIT"},
		ModuleExceptions: []ModuleException{{Module: "excepted/mod", License: "LGPL-3.0"}},
	}
	filter := policy.Filter()

	if !permitsHit(filter, "excepted/mod", LicenseHit{RelPath: "LICENSE", License: "LGPL-3.0"}) {
		t.Error("the exception must cover the module's own root grant — that is the file it was verified against")
	}
	if permitsHit(filter, "excepted/mod", LicenseHit{RelPath: "testdata/COPYING", License: "LGPL-3.0"}) {
		t.Error("the exception must NOT reach a nested file nobody verified")
	}
	if !permitsHit(filter, "excepted/mod", LicenseHit{RelPath: "vendor/dep/LICENSE", License: "MIT"}) {
		t.Error("an allowlisted license is permissive at any depth and needs no exception")
	}
	if permitsHit(filter, "other/mod", LicenseHit{RelPath: "LICENSE", License: "LGPL-3.0"}) {
		t.Error("an exception is keyed to its module and must not cover another one")
	}
}

// TestSentinelLicensesHaveNoPathThroughAnyFilter pins links-licensing-c0ce.9's
// hard rule at the graph audit's ruling site: a license this tool could not
// read is not permitted by ANY policy, however that policy was written.
//
// The policy built here is the most permissive one that can be expressed — it
// allowlists a sentinel outright AND grants the module a root-grant exception
// for it. parsePolicy refuses such a FILE, so it is assembled as a value, and
// that is the case that matters: Filter copies a policy's entries verbatim, so
// the sentinel really is a live key in both of the returned filter's tables.
// The ban lives at the rulings (Allows, Permits) instead, which is what makes
// it hold here rather than depending on the file having been read through the
// parse.
//
// An earlier draft of this test also built a filter by hand to cover "a
// LicenseFilter nobody parsed a file to get". Round 2 of review pointed out
// that once Filter stopped dropping keys, the hand-built value was identical
// to policy.Filter() and the two halves could never disagree — a second
// assertion that could only ever repeat the first. The single filter below IS
// the adversarial state. [LAW:single-enforcer]
func TestSentinelLicensesHaveNoPathThroughAnyFilter(t *testing.T) {
	for _, sentinel := range []string{unclassifiedLicense, oversizeLicense} {
		policy := &Policy{
			AllowedLicenses: []string{"MIT", sentinel},
			ModuleExceptions: []ModuleException{
				{Module: "excepted/mod", License: sentinel, Reason: "a reason nobody could have had"},
			},
		}
		// The rulings must agree with the parse about WHAT THE SENTINEL IS.
		// They disagreed for a commit — refuseSentinel folded ASCII case while
		// Allows did an exact map lookup — so a filter holding "unknown"
		// permitted it, under a doc paragraph promising the ban holds for
		// every LicenseFilter.
		folded := LicenseFilter{allowed: map[string]bool{strings.ToLower(sentinel): true}}
		if folded.Allows(strings.ToLower(sentinel)) {
			t.Errorf("%q: a case variant of the sentinel is permitted by Allows, though the parse calls that spelling the sentinel", sentinel)
		}
		filter := policy.Filter()
		if !filter.allowed[sentinel] || !filter.excepted[exKey{module: "excepted/mod", license: sentinel}] {
			t.Fatalf("%q: the fixture is meant to put the sentinel in BOTH tables; if Filter has started dropping it, this test is no longer testing the rulings", sentinel)
		}
		if filter.Allows(sentinel) {
			t.Errorf("%q: allowlisting a sentinel must not make it allowed — it names no grant", sentinel)
		}
		if filter.Permits("excepted/mod", sentinel) {
			t.Errorf("%q: an exception must not reach a sentinel — it records a reading of a license that could not be read", sentinel)
		}
		if permitsHit(filter, "excepted/mod", LicenseHit{RelPath: "LICENSE", License: sentinel}) {
			t.Errorf("%q: the graph audit must report a sentinel at a module's root, never suppress it", sentinel)
		}
	}
}

// TestPartitionGraphFilesBothSentinelsAsUnclassified pins the routing half of
// links-licensing-c0ce.9's one-source-of-truth refactor: partitionGraph reads
// licenseSentinels rather than re-listing the two constants, and BOTH of them
// must land in the unclassified section rather than under module grants.
//
// The oversize half was pinned by nothing until round 4 of review — delete it
// from the map and every test stayed green while "Skipped (oversize)" started
// being reported as a module's own license GRANT, which is a row that reads as
// a legal finding about a file the tool declined to open.
func TestPartitionGraphFilesBothSentinelsAsUnclassified(t *testing.T) {
	filter := (&Policy{AllowedLicenses: []string{"MIT"}}).Filter()
	for _, sentinel := range []string{unclassifiedLicense, oversizeLicense} {
		sections := partitionGraph([]GraphEntry{{
			Module: Module{Path: "murky/mod", Version: "v1"},
			Hits:   []LicenseHit{{RelPath: "LICENSE", License: sentinel}},
		}}, filter)
		var found string
		for _, sec := range sections {
			for _, r := range sec.Rows {
				if r.License == sentinel {
					found = sec.Title
				}
			}
		}
		if found != sectionUnclassified {
			t.Errorf("%q at a module root was filed under %q, want %q — a sentinel is the tool having no verdict, never a grant",
				sentinel, found, sectionUnclassified)
		}
	}
}

// TestPartitionGraphReportsReplacedModulesRegardlessOfLicense pins the one
// section that is not about a violation: a replaced module is listed because
// its coordinate and its source disagree, which stays true when the license is
// perfectly permissive. This repo's dolt fork is Apache-2.0 at both ends and
// must still be reported. [LAW:no-silent-failure]
func TestPartitionGraphReportsReplacedModulesRegardlessOfLicense(t *testing.T) {
	policy := &Policy{AllowedLicenses: []string{"Apache-2.0"}}
	entries := []GraphEntry{{
		Module: Module{Path: "github.com/dolthub/dolt/go", Version: "v0.40.5", Replacement: Replacement{Kind: ReplacedByFork, Path: "github.com/promptctl/dolt/go", Version: "v0.40.5-later"}},
		Hits:   []LicenseHit{{RelPath: "LICENSE", License: "Apache-2.0"}},
	}}

	for _, s := range partitionGraph(entries, policy.Filter()) {
		if s.Title != sectionReplaced {
			continue
		}
		if len(s.Rows) != 1 {
			t.Fatalf("want the replaced module reported, got %+v", s.Rows)
		}
		if !strings.Contains(s.Rows[0].Path, "promptctl") {
			t.Errorf("replacement row must name where the source came from, got %q", s.Rows[0].Path)
		}
		if s.Rows[0].License != "Apache-2.0" {
			t.Errorf("replacement row must carry the root grant it read, got %q", s.Rows[0].License)
		}
		return
	}
	t.Fatal("no replaced-modules section in the report")
}

// TestRootGrantLicenseNamesAmbiguity pins that a module with several root-level
// license files is reported as ambiguous rather than having one picked for it.
// github.com/opencontainers/go-digest is the real instance: Apache-2.0 in
// LICENSE and CC-BY-SA-4.0 in LICENSE.docs.
func TestRootGrantLicenseNamesAmbiguity(t *testing.T) {
	if got := rootGrantLicense(nil); got != "(no root grant)" {
		t.Errorf("no hits: got %q", got)
	}
	if got := rootGrantLicense([]LicenseHit{{RelPath: "LICENSE", License: "MIT"}}); got != "MIT" {
		t.Errorf("single root grant: got %q, want MIT", got)
	}
	two := []LicenseHit{
		{RelPath: "LICENSE", License: "Apache-2.0"},
		{RelPath: "LICENSE.docs", License: "CC-BY-SA-4.0"},
	}
	if got := rootGrantLicense(two); !strings.Contains(got, "more root files") {
		t.Errorf("two root grants must be reported as ambiguous, got %q", got)
	}
	// A nested hit is not a root grant and must not be mistaken for one.
	nested := []LicenseHit{{RelPath: "licenses/gpl.txt", License: "GPL-2.0"}}
	if got := rootGrantLicense(nested); got != "(no root grant)" {
		t.Errorf("nested-only module: got %q, want no root grant", got)
	}
}

// TestGraphAuditCoversWholeBuildList IS this ticket's acceptance criterion
// (links-licensing-c0ce.1) expressed as a test, run against the real graph
// rather than a fixture: every module `go list -m all` resolves is classified,
// none is skipped for want of a local copy, and the known findings are present.
//
// The freetype assertion is the one that would have failed against a top-level
// scan: that module's root LICENSE is a pointer document the classifier cannot
// match, and its GPL text lives at licenses/gpl.txt — a path no license-shaped
// filename pattern reaches.
func TestGraphAuditCoversWholeBuildList(t *testing.T) {
	requireWholeGraph(t)

	entries, err := buildGraphEntries()
	if err != nil {
		t.Fatalf("buildGraphEntries: %v", err)
	}
	if len(entries) < 500 {
		t.Fatalf("got %d modules; lit's graph is hundreds of modules — resolution looks broken", len(entries))
	}

	// [LAW:verifiable-goals] "no module unclassified for want of a local copy"
	// is the acceptance criterion, so assert the directory is really there
	// rather than trusting that `go mod download all` reported success.
	for _, e := range entries {
		if e.Module.Dir == "" {
			t.Fatalf("%s@%s has no module directory — it was never fetched", e.Module.Path, e.Module.Version)
		}
		if _, err := os.Stat(e.Module.Dir); err != nil {
			t.Fatalf("%s@%s: module directory unusable: %v", e.Module.Path, e.Module.Version, err)
		}
	}

	byPath := make(map[string]GraphEntry, len(entries))
	for _, e := range entries {
		byPath[e.Module.Path] = e
	}

	freetype, ok := byPath["github.com/golang/freetype"]
	if !ok {
		t.Fatal("github.com/golang/freetype absent from the graph; this test's premise is stale")
	}
	var foundGPL bool
	for _, h := range freetype.Hits {
		if h.RelPath == filepath.Join("licenses", "gpl.txt") && h.License == "GPL-2.0" {
			foundGPL = true
		}
	}
	if !foundGPL {
		t.Errorf("freetype's licenses/gpl.txt was not classified as GPL-2.0; hits: %+v", freetype.Hits)
	}
}

// TestGraphAuditLeavesGoSumUntouched pins that the audit does not modify the
// repository it audits.
//
// This is not tidiness. `go mod download all` fetches modules no build needs
// and records each one's zip hash in go.sum — 330 lines against this repo,
// which the next `go mod tidy` strips right back out, putting the audit and
// tidy in a loop that each undoes. go.sum is also the cache key for CI's Go
// build cache, so an audit that rewrote it would invalidate a multi-gigabyte
// cache on every run.
//
// The regression this guards against is specific and was hit while building
// this: restoring go.sum between the download and `go list` looks equivalent
// and is not, because `go list` verifies a module against go.sum before
// reporting its directory — so a too-eager restore leaves freshly fetched
// modules present on disk with an empty .Dir, and the audit goes blind exactly
// where it was supposed to be looking. [LAW:no-silent-failure]
func TestGraphAuditLeavesGoSumUntouched(t *testing.T) {
	requireWholeGraph(t)

	const goSum = "../../go.sum"
	before, err := os.ReadFile(goSum)
	if err != nil {
		t.Fatalf("read go.sum: %v", err)
	}

	mods, err := GraphModules()
	if err != nil {
		t.Fatalf("GraphModules: %v", err)
	}

	after, err := os.ReadFile(goSum)
	if err != nil {
		t.Fatalf("re-read go.sum: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the graph audit modified go.sum (%d bytes before, %d after)", len(before), len(after))
	}

	// The other half of the same invariant: preserving go.sum must not cost
	// the audit its sight. Every module must still have resolved a directory.
	for _, m := range mods {
		if m.Dir == "" {
			t.Fatalf("%s@%s resolved no directory — go.sum was restored too early", m.Path, m.Version)
		}
	}
}

// TestGraphModulesAreDeterministic pins the resolution half of "the same answer
// twice", which is the audit's whole value: a number in a ticket stops being
// something a human measured once only if the tool agrees with itself.
//
// It exercises GraphModules rather than the full buildGraphEntries because the
// two places order could vary are the map parseModuleList deduplicates through
// (covered here) and WalkDir's traversal (covered by
// TestScanLicenseTextsIsSorted). Re-scanning 588 module trees a second time
// would add half a minute to every CI run to re-prove what those two already
// establish. [LAW:behavior-not-structure]
func TestGraphModulesAreDeterministic(t *testing.T) {
	requireWholeGraph(t)

	first, err := GraphModules()
	if err != nil {
		t.Fatalf("GraphModules: %v", err)
	}
	second, err := GraphModules()
	if err != nil {
		t.Fatalf("GraphModules: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("module count differs across runs: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("module %d differs across runs: %+v vs %+v", i, first[i], second[i])
		}
	}
}

// TestGraphReportRendersEverySection pins that the report names all five
// sections even when a section is empty, so a reader can tell "this audit found
// nothing here" from "this audit does not look here". [LAW:no-silent-failure]
func TestGraphReportRendersEverySection(t *testing.T) {
	policy := &Policy{AllowedLicenses: []string{"MIT"}}
	entries := []GraphEntry{{Module: Module{Path: "clean", Version: "v1"}, Hits: []LicenseHit{{RelPath: "LICENSE", License: "MIT"}}}}

	var b strings.Builder
	if err := WriteGraphReport(&b, entries, policy.Filter()); err != nil {
		t.Fatalf("WriteGraphReport: %v", err)
	}
	out := b.String()
	for _, title := range []string{
		sectionReplaced, sectionModuleGrants, sectionNestedTexts, sectionUnclassified, sectionNoLicense,
	} {
		if !strings.Contains(out, title) {
			t.Errorf("report omits the %q section entirely:\n%s", title, out)
		}
	}
	if !strings.Contains(out, "none") {
		t.Error("an empty section must say so explicitly rather than rendering blank")
	}
	if !strings.Contains(out, "1 modules in the go.mod build list") {
		t.Errorf("report must state how much was measured:\n%s", out)
	}
}
