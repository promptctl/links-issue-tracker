package main

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	lc "github.com/google/licenseclassifier"
)

// verifiedNativeEntries is nativeEntries with the classifier construction and
// error handling its callers would otherwise each repeat. Every test that wants
// the native Entry values goes through the real verification on the way, so a
// curated license literal drifting from its notice text fails the whole native
// suite rather than only the one test aimed at it.
func verifiedNativeEntries(t *testing.T) []Entry {
	t.Helper()
	classifier, err := lc.New(lc.DefaultConfidenceThreshold)
	if err != nil {
		t.Fatalf("build license classifier: %v", err)
	}
	entries, err := nativeEntries(classifier)
	if err != nil {
		t.Fatalf("nativeEntries: %v", err)
	}
	return entries
}

// TestNativeLibsInSBOMAndBundle is this ticket's acceptance criterion: the SBOM
// and the attribution bundle both list ICU, zstd, and musl (and compiler-rt)
// with a license and a version, and the bundle carries their notice text. It
// renders from nativeEntries — the exact Entry values buildEntries appends to
// the Go inventory — so it checks precisely what the generated artifacts contain.
func TestNativeLibsInSBOMAndBundle(t *testing.T) {
	entries := verifiedNativeEntries(t)

	// Bundle: each native lib's name/version section plus its verbatim notice.
	var bundle bytes.Buffer
	if err := WriteBundle(&bundle, entries); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	for _, want := range []string{"icu 75.1", "zstd 1.5.6", "musl 1.2.5", "compiler-rt 0.14.0"} {
		if !strings.Contains(bundle.String(), want) {
			t.Errorf("bundle missing native lib %q", want)
		}
	}
	// Notice text (not just the name) must ship — the attribution obligation.
	// A UNIQUE substring per lib, so each embedded text is independently
	// verified: "MIT license" alone would match musl's COPYRIGHT and let
	// compiler-rt's distinct "The MIT License (Expat)" go unchecked.
	// compiler-rt's notice is checked twice because its expression is a
	// compound (MIT AND Apache-2.0 WITH LLVM-exception): the Zig MIT text and
	// the LLVM license text must BOTH ship, or the attribution is short one arm.
	for _, want := range []string{
		"UNICODE LICENSE V3",          // icu
		"Zstandard",                   // zstd
		"musl as a whole is licensed", // musl
		"The MIT License (Expat)",     // compiler-rt (zig's own license)
		"The LLVM Project is under the Apache License v2.0 with LLVM Exceptions", // compiler-rt (ported LLVM routines)
	} {
		if !strings.Contains(bundle.String(), want) {
			t.Errorf("bundle missing native notice text %q", want)
		}
	}

	// SBOM: each native lib is a component with a version, a pkg:generic purl,
	// and a license.
	bom := decodeSBOM(t, entries, "")
	byName := map[string]int{}
	for i, c := range bom.Components {
		byName[c.Name] = i
	}
	// The license columns are the two CycloneDX arms, asserted together for
	// every row: a single SPDX id belongs in license.name, a compound grant in
	// the sibling expression field. Both are pinned because a value in the
	// wrong arm is invisible to a scanner reading the other one — compiler-rt's
	// compound left in license.name reads as one license literally named with
	// an AND in it, and an SPDX-aware policy engine sees no grant at all.
	for _, tc := range []struct{ name, version, licenseName, expression string }{
		{"icu", "75.1", "Unicode-3.0", ""},
		{"zstd", "1.5.6", "BSD-3-Clause", ""},
		{"musl", "1.2.5", "MIT", ""},
		{"compiler-rt", "0.14.0", "", "MIT AND Apache-2.0 WITH LLVM-exception"},
	} {
		i, ok := byName[tc.name]
		if !ok {
			t.Errorf("SBOM missing native component %q", tc.name)
			continue
		}
		c := bom.Components[i]
		if c.Version != tc.version {
			t.Errorf("%s version = %q, want %q", tc.name, c.Version, tc.version)
		}
		if c.PURL != "pkg:generic/"+tc.name+"@"+tc.version {
			t.Errorf("%s purl = %q, want pkg:generic/%s@%s", tc.name, c.PURL, tc.name, tc.version)
		}
		if len(c.Licenses) != 1 {
			t.Errorf("%s licenses = %+v, want exactly one entry", tc.name, c.Licenses)
			continue
		}
		if got := c.Licenses[0].License.Name; got != tc.licenseName {
			t.Errorf("%s license.name = %q, want %q", tc.name, got, tc.licenseName)
		}
		if got := c.Licenses[0].Expression; got != tc.expression {
			t.Errorf("%s license expression = %q, want %q", tc.name, got, tc.expression)
		}
	}

	// The two rows lit concluded rather than read off a notice must say so in
	// the field CycloneDX defines for it. zstd is the sharp case: upstream
	// declares BSD-3-Clause OR GPL-2.0-only, so a scanner resolving zstd 1.5.6
	// on its own and finding lit's single BSD-3-Clause row needs to see this
	// marked as an election, not read it as a contradiction of the dual grant.
	for _, tc := range []struct{ name, want string }{
		{"zstd", "concluded"},        // elected one arm of a dual license
		{"compiler-rt", "concluded"}, // expression composed from the ported sources' headers
		{"icu", ""},                  // upstream's own grant, reported as-is
		{"musl", ""},
	} {
		choice := bom.Components[byName[tc.name]].Licenses[0]
		// Read the field the arm dictates, never "whichever of the two is
		// set". The two are not interchangeable: CycloneDX 1.6's name arm is
		// additionalProperties:false with `license` as its ONLY permitted key,
		// so an acknowledgement beside a license object is schema-invalid,
		// while on the expression arm it is a permitted sibling. An
		// OR-of-both read is satisfied by either placement, so it stays green
		// through precisely the refactor that breaks the document — and
		// cyclonedx-cli, the thing that would catch it, runs only at tag-cut.
		got, where := choice.License.Acknowledgement, "license.acknowledgement"
		if choice.Expression != "" {
			got, where = choice.Acknowledgement, "the expression arm's sibling acknowledgement"
		} else if choice.Acknowledgement != "" {
			t.Errorf("%s: acknowledgement %q sits beside `license`, where CycloneDX 1.6 permits no key but `license`; it belongs inside the license object",
				tc.name, choice.Acknowledgement)
		}
		if got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.name, where, got, tc.want)
		}
	}
}

// TestLicenseChoiceArms is the accept/reject table for the one decision that
// decides whether a compound grant is machine-readable: which CycloneDX arm a
// license value lands in. The rows that matter are the near-misses — an
// identifier that spells an operator inside itself must stay a name, or a
// substring check would quietly file GPL-2.0-or-later as an expression and
// every SPDX-aware consumer would stop seeing it. LicenseName is not a curated
// set; it carries whatever licenseclassifier returns for every linked module.
func TestLicenseChoiceArms(t *testing.T) {
	for _, tc := range []struct{ in, wantName, wantExpression string }{
		{"MIT", "MIT", ""},
		{"Unicode-3.0", "Unicode-3.0", ""},
		{"BSD-3-Clause", "BSD-3-Clause", ""},
		{"GPL-2.0-or-later", "GPL-2.0-or-later", ""},
		{"MIT AND Apache-2.0 WITH LLVM-exception", "", "MIT AND Apache-2.0 WITH LLVM-exception"},
		{"BSD-3-Clause OR GPL-2.0-only", "", "BSD-3-Clause OR GPL-2.0-only"},
		{"Apache-2.0 WITH LLVM-exception", "", "Apache-2.0 WITH LLVM-exception"},
	} {
		got := licenseChoice(tc.in, "")
		name := ""
		if got.License != nil {
			name = got.License.Name
		}
		if name != tc.wantName || got.Expression != tc.wantExpression {
			t.Errorf("licenseChoice(%q) = name %q / expression %q, want name %q / expression %q",
				tc.in, name, got.Expression, tc.wantName, tc.wantExpression)
		}
	}
}

// TestNativeNotesSurfaceInReportAndSBOM is links-licensing-c0ce.8's acceptance
// criterion for the curated notes: zstd's dual-license election and
// compiler-rt's compound-expression provenance must be readable in the shipped
// artifacts themselves — LICENSE-REPORT.md's Notes section and the SBOM — not
// only in native.go, which ships to nobody.
//
// The SBOM half of that criterion is asserted against the note's home as of
// links-licensing-c0ce.15, which is a namespaced property rather than
// component.description. The criterion itself did not change: the note must
// reach a reader of the shipped document. What changed is which field carries
// it, because CycloneDX defines description as what the component IS.
func TestNativeNotesSurfaceInReportAndSBOM(t *testing.T) {
	entries := verifiedNativeEntries(t)

	var report bytes.Buffer
	if err := WriteReport(&report, entries); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if !strings.Contains(report.String(), "## Notes") {
		t.Error("report missing the Notes section")
	}
	for _, want := range []string{
		"lit elects and distributes under BSD-3-Clause",            // zstd election
		"ported from LLVM compiler-rt sources licensed Apache-2.0", // compiler-rt provenance
		"lit elects and distributes those under MIT",               // compiler-rt's pre-relicense NCSA/MIT election
	} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("report notes missing %q", want)
		}
	}

	bom := decodeSBOM(t, entries, "")
	notes := map[string]string{}
	for _, c := range bom.Components {
		note, ok := c.property(licenseNoteProperty)
		if !ok {
			t.Errorf("native component %s carries no %s property; its curated note reaches no reader of the SBOM", c.Name, licenseNoteProperty)
		}
		notes[c.Name] = note
	}
	if !strings.Contains(notes["zstd"], "BSD-3-Clause OR GPL-2.0-only") {
		t.Errorf("zstd SBOM note does not state the dual-license election: %q", notes["zstd"])
	}
	if !strings.Contains(notes["compiler-rt"], "Apache-2.0 WITH LLVM-exception") {
		t.Errorf("compiler-rt SBOM note does not state the ported-LLVM provenance: %q", notes["compiler-rt"])
	}
	// ICU and musl keep a single SPDX identifier while bundling third-party
	// material of their own, so each must disclose what its notice folds in —
	// otherwise a reader who fingerprints TRE inside musl, or reads the GPL
	// blocks in ICU's LICENSE, has no document from us.
	if !strings.Contains(notes["musl"], "2-clause BSD") {
		t.Errorf("musl SBOM note does not disclose its third-party components: %q", notes["musl"])
	}
	if !strings.Contains(notes["icu"], "autotools build scripts") {
		t.Errorf("icu SBOM note does not account for the GPL blocks in its LICENSE: %q", notes["icu"])
	}

	// Every note must reach the attribution bundle too. That is the file which
	// legally accompanies the binary, and it is where an election matters most:
	// compiler-rt's verbatim text tells the recipient they may choose either
	// license, so without the note nothing there says which one lit chose.
	var bundle bytes.Buffer
	if err := WriteBundle(&bundle, entries); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	for _, n := range nativeLibs {
		// The empty note is checked explicitly, not left to the Contains below.
		// "Note: "+"" is just "Note: ", which any other lib's note satisfies —
		// so a native lib added without one would sail through this loop while
		// nothing in THIRD_PARTY_LICENSES documented it. Every curated native
		// lib carries a note; that is the inventory's contract, asserted here.
		if n.note == "" {
			t.Errorf("native lib %s has no note; every curated native lib must disclose what its notice covers", n.name)
			continue
		}
		if !strings.Contains(bundle.String(), "Note: "+n.note) {
			t.Errorf("THIRD_PARTY_LICENSES is missing %s's note", n.name)
		}
	}
}

// TestBundleOmitsEmptyNote pins the other half of noteLine's contract: an entry
// with no curated note renders no note line at all. Every one of the ~149 Go
// modules is note-free, so a regression here would stamp a bare "Note:" onto
// each of their sections in the shipped attribution file.
func TestBundleOmitsEmptyNote(t *testing.T) {
	var bundle bytes.Buffer
	if err := WriteBundle(&bundle, []Entry{
		{Module: Module{Path: "example.com/plain", Version: "v1.0.0"}, LicenseName: "MIT", Text: "MIT text"},
	}); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if strings.Contains(bundle.String(), "Note:") {
		t.Errorf("note-free entry emitted a Note line:\n%s", bundle.String())
	}
}

// TestNativeLibsPassPolicy confirms the license-policy gate accepts every native
// library — the ticket requires the gate to "account for them". Runs the real
// predicate over just the native inventory against the committed policy.
func TestNativeLibsPassPolicy(t *testing.T) {
	policy, err := LoadPolicy()
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if v := CheckPolicy(verifiedNativeEntries(t), policy); len(v) != 0 {
		t.Errorf("native libs violate policy (allowlist their license in policy.json): %+v", v)
	}
}

// dockerfileARG extracts `ARG NAME=value` from build/Dockerfile.release.
var dockerfileARG = func() func(name string) string {
	data, err := os.ReadFile("../../build/Dockerfile.release")
	return func(name string) string {
		if err != nil {
			return ""
		}
		m := regexp.MustCompile(`(?m)^ARG ` + regexp.QuoteMeta(name) + `=(\S+)`).FindSubmatch(data)
		if m == nil {
			return ""
		}
		return string(m[1])
	}
}()

// TestNativeICUVersionMatchesDockerfile pins the one native version that lives
// in-repo: build/Dockerfile.release's ICU_MAJOR.ICU_MINOR is the source of truth
// for which ICU is cross-built into the release image, and native.go hardcodes
// the same version for the SBOM/bundle. This makes that coupling checked — bump
// ICU in the Dockerfile without updating native.go and the build fails here
// instead of shipping an SBOM that names the wrong ICU version.
// [LAW:one-source-of-truth]
func TestNativeICUVersionMatchesDockerfile(t *testing.T) {
	major, minor := dockerfileARG("ICU_MAJOR"), dockerfileARG("ICU_MINOR")
	if major == "" || minor == "" {
		t.Fatalf("could not read ICU_MAJOR/ICU_MINOR from build/Dockerfile.release (got %q/%q)", major, minor)
	}
	want := major + "." + minor
	var got string
	for _, n := range nativeLibs {
		if n.name == "icu" {
			got = n.version
		}
	}
	if got != want {
		t.Errorf("native.go icu version = %q, but build/Dockerfile.release pins ICU %q; update native.go to match", got, want)
	}
}

// TestNativeZigVersionMatchesDockerfile pins compiler-rt's version to the zig
// toolchain that provides it: build/Dockerfile.release's ZIG_VERSION is the
// source of truth, and musl/compiler-rt are what that zig bundles. A zig bump
// can change the bundled musl/compiler-rt, so at least the zig-versioned
// compiler-rt component is coupled to it here. [LAW:one-source-of-truth]
func TestNativeZigVersionMatchesDockerfile(t *testing.T) {
	zig := strings.TrimPrefix(dockerfileARG("ZIG_VERSION"), "v")
	if zig == "" {
		t.Fatal("could not read ZIG_VERSION from build/Dockerfile.release")
	}
	var got string
	for _, n := range nativeLibs {
		if n.name == "compiler-rt" {
			got = n.version
		}
	}
	if got != zig {
		t.Errorf("native.go compiler-rt version = %q, but build/Dockerfile.release pins ZIG_VERSION %q; update native.go (and re-confirm the bundled musl version) when zig changes", got, zig)
	}
}

// TestNativeDescriptionsSayWhatTheComponentIs pins the split
// links-licensing-c0ce.15 made between the two claims a native component
// carries. Before it, the curated licensing note rode in component.description,
// which CycloneDX defines as what the component IS — a true statement in a field
// that does not mean what the statement says, and one merge and dedup tooling
// treats as identity metadata.
//
// Two assertions in the loop catch a restored `Description: e.Note`, and they
// catch different things. The equality check fails because the description is
// no longer the curated one; the separation check below it fails because the
// description IS the note. Only the second still fires if someone "fixes" the
// first by copying the note into the description field as well, which is why
// both are here and why neither is redundant.
//
// The separation check is guarded on a non-empty note. `strings.Contains(x, "")`
// is true for every x, so an unguarded form would fire spuriously against the
// first native lib that legitimately needs no licensing note — reporting that
// its description "carries the licensing note" while naming a note that does
// not exist.
func TestNativeDescriptionsSayWhatTheComponentIs(t *testing.T) {
	byName := componentsByName(decodeSBOM(t, verifiedNativeEntries(t), ""))

	for _, n := range nativeLibs {
		c, ok := byName[n.name]
		if !ok {
			t.Errorf("native lib %s produced no SBOM component", n.name)
			continue
		}
		// A native lib's name is a bare string, not a resolvable coordinate, so
		// a reader of the SBOM alone has nothing else telling them what "musl"
		// or "compiler-rt" are. That is the gap description exists to close, and
		// an empty one leaves it open.
		if n.description == "" {
			t.Errorf("native lib %s has no description; the SBOM would name it without saying what it is", n.name)
			continue
		}
		if c.Description != n.description {
			t.Errorf("%s component.description = %q, want the curated description %q", n.name, c.Description, n.description)
		}
		if n.note != "" && strings.Contains(c.Description, n.note) {
			t.Errorf("%s component.description carries the licensing note; CycloneDX defines description as what the component IS, and the note belongs in the %s property: %q",
				n.name, licenseNoteProperty, c.Description)
		}
	}
}

// TestGoModulesCarryNoDescriptionOrNote pins the other half of the split: only
// the curated native inventory has either field. A Go module's description and
// note are both empty, so its component omits description and properties
// entirely rather than emitting a fabricated sentence about a module lit never
// described. [LAW:no-silent-failure] the absence is real, not filled in.
func TestGoModulesCarryNoDescriptionOrNote(t *testing.T) {
	bom := decodeSBOM(t, synthEntries, "")
	for _, c := range bom.Components {
		if c.Description != "" {
			t.Errorf("Go module %s carries description %q; lit holds no description for a Go module that it did not invent", c.Name, c.Description)
		}
		if _, ok := c.property(licenseNoteProperty); ok {
			t.Errorf("Go module %s carries a %s property; only the curated native inventory has notes", c.Name, licenseNoteProperty)
		}
	}
}

// TestNativeDescriptionsMakeNoPlatformClaim pins the rule musl's description was
// corrected to obey. The curated inventory is generated once per release and
// rendered into artifacts that are not platform-specific — LICENSE-REPORT.md
// ships inside EVERY archive goreleaser builds, under a preamble asserting the
// components listed are compiled into this binary, and the SBOM is a single
// standalone asset covering every platform. A description scoped to one of them
// ("linked into lit's fully-static Linux builds") is a false sentence in the
// copies a darwin or windows recipient opens.
//
// The check is deliberately a keyword scan, because the mistake it catches is a
// keyword: a maintainer adds a platform name to a description that ships
// everywhere. (The first version of this comment justified the rule by saying
// the SBOM ships inside every archive. It does not ship in any archive — it is
// staged separately — and the rule survives on the report, which does.) Describing WHAT a component is never requires naming an operating
// system; if a future component genuinely needs that qualification, it belongs
// in the note, which is about lit's use of the component rather than about the
// component itself.
func TestNativeDescriptionsMakeNoPlatformClaim(t *testing.T) {
	for _, n := range nativeLibs {
		for _, platform := range []string{"Linux", "linux", "darwin", "Darwin", "macOS", "Windows", "windows"} {
			if strings.Contains(n.description, platform) {
				t.Errorf("native lib %s describes itself in terms of %q, but one SBOM ships in every platform's archive: %q",
					n.name, platform, n.description)
			}
		}
	}
}

// nativeLibNamed returns the curated record for name. The value is a copy, but
// its slices share the package record's backing arrays, so a test mutating it
// goes through withFindings/withEvidence rather than writing through an index.
func nativeLibNamed(t *testing.T, name string) nativeLib {
	t.Helper()
	for _, n := range nativeLibs {
		if n.name == name {
			return n
		}
	}
	t.Fatalf("no native lib named %q", name)
	return nativeLib{}
}

// withFindings returns a copy of n whose findings are edit applied to a FRESH
// copy of n's own. The copy is the point: an in-place write would corrupt the
// package-level record for every test that runs after, and the corruption would
// surface as an unrelated failure somewhere else in the suite.
func withFindings(n nativeLib, edit func([]noticeFinding) []noticeFinding) nativeLib {
	n.findings = edit(append([]noticeFinding(nil), n.findings...))
	return n
}

// withEvidence is withFindings for the evidence list, and for the same reason.
func withEvidence(n nativeLib, edit func([]armEvidence) []armEvidence) nativeLib {
	n.evidence = edit(append([]armEvidence(nil), n.evidence...))
	return n
}

// TestVerifyNoticeCatchesEachWayTheRecordCanLie is the mutation proof for the
// rule that gives the four curated license literals teeth. Every row breaks
// exactly ONE guarantee in an otherwise-correct record and names the diagnostic
// that must fire, because a check whose rows all pass for the same reason
// proves only that the check is not empty. A row asserting merely "some error"
// would stay green if half these rules were deleted.
//
// The first row is the scenario the ticket was written from: zstd relicenses,
// a maintainer writes the new identifier into native.go and adds it to
// allowed_licenses, and every rule in policy.go passes it because the
// classifier's taxonomy has no opinion about a 2021-corpus-absent license.
// What stops it is not a bigger taxonomy — it is that the notice bytes still
// say BSD-3-Clause and now nothing in the record matches them.
func TestVerifyNoticeCatchesEachWayTheRecordCanLie(t *testing.T) {
	classifier, err := lc.New(lc.DefaultConfidenceThreshold)
	if err != nil {
		t.Fatalf("build license classifier: %v", err)
	}
	icu, zstd, compilerRT := nativeLibNamed(t, "icu"), nativeLibNamed(t, "zstd"), nativeLibNamed(t, "compiler-rt")

	relicensed := zstd
	relicensed.license = "BUSL-1.1"

	blankCovers := withFindings(zstd, func(f []noticeFinding) []noticeFinding {
		f[0].covers = "   "
		return f
	})
	zeroCount := withFindings(zstd, func(f []noticeFinding) []noticeFinding {
		f[0].count = 0
		return f
	})
	duplicated := withFindings(zstd, func(f []noticeFinding) []noticeFinding {
		return append(f, f[0])
	})
	bundledArm := withFindings(zstd, func(f []noticeFinding) []noticeFinding {
		f[0].role = roleBundled
		return f
	})
	stale := withFindings(zstd, func(f []noticeFinding) []noticeFinding {
		return append(f, noticeFinding{identifier: "ISC", count: 1, role: roleBundled, covers: "nothing at all"})
	})
	redundantEvidence := withEvidence(zstd, func(e []armEvidence) []armEvidence {
		return append(e, armEvidence{token: "BSD-3-Clause", phrase: "BSD License"})
	})
	strayEvidence := withEvidence(zstd, func(e []armEvidence) []armEvidence {
		return append(e, armEvidence{token: "MIT", phrase: "BSD License"})
	})

	textGainedALicense := withFindings(icu, func(f []noticeFinding) []noticeFinding {
		out := f[:0]
		for _, x := range f {
			if x.identifier != "GPL-3.0" {
				out = append(out, x)
			}
		}
		return out
	})
	countDrift := withFindings(icu, func(f []noticeFinding) []noticeFinding {
		for i := range f {
			if f[i].identifier == "BSD-3-Clause" {
				f[i].count = 4
			}
		}
		return f
	})
	copyleftCalledBundled := withFindings(icu, func(f []noticeFinding) []noticeFinding {
		for i := range f {
			if f[i].identifier == "GPL-3.0" {
				f[i].role = roleBundled
			}
		}
		return f
	})
	phraseNotInText := withEvidence(icu, func(e []armEvidence) []armEvidence {
		e[0].phrase = "UNICODE LICENSE V9"
		return e
	})

	droppedException := withEvidence(compilerRT, func([]armEvidence) []armEvidence { return nil })

	for _, tc := range []struct {
		name string
		lib  nativeLib
		want []string
	}{
		{"a relicense the notice bytes do not support", relicensed, []string{
			`nothing corroborates its "BUSL-1.1"`,
			`declares "BSD-3-Clause" as a grant lit distributes under, but it is not an arm`,
		}},
		{"the text gained a license nobody declared", textGainedALicense, []string{
			`the classifier finds "GPL-3.0" in the embedded notice text 2 times, and this record says nothing about it`,
		}},
		{"the text gained material of a kind already present", countDrift, []string{
			`declares "BSD-3-Clause" 4 times, but the classifier finds it 5 times`,
		}},
		{"a declaration the text no longer supports", stale, []string{
			`declares "ISC", which the classifier no longer finds`,
		}},
		{"copyleft laundered as bundled material", copyleftCalledBundled, []string{
			`declares "GPL-3.0" as material bundled into the shipped library`,
			`types it "restricted"`,
			`a component to remove`,
		}},
		{"an arm demoted to bundled material", bundledArm, []string{
			`declares "BSD-3-Clause" as bundled third-party material while the curated license`,
		}},
		{"an exception nothing vouches for", droppedException, []string{
			`nothing corroborates its "LLVM-exception"`,
		}},
		{"evidence quoting a phrase the text does not carry", phraseNotInText, []string{
			`quotes "UNICODE LICENSE V9", which does not appear in the embedded notice text`,
		}},
		{"evidence duplicating what a finding already proves", redundantEvidence, []string{
			`carries evidence for "BSD-3-Clause", which a roleGranted finding already corroborates`,
		}},
		{"evidence for a token that is not an arm", strayEvidence, []string{
			`carries evidence for "MIT", which is not a half of any arm`,
		}},
		{"a finding that names no material", blankCovers, []string{
			`declares "BSD-3-Clause" without naming the material it licenses`,
		}},
		{"a finding claiming zero occurrences", zeroCount, []string{
			`declares "BSD-3-Clause" with a count of 0`,
		}},
		{"the same identifier declared twice", duplicated, []string{
			`declares "BSD-3-Clause" twice`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.lib.verifyNotice(classifier)
			if err == nil {
				t.Fatalf("verifyNotice accepted a record that %s", tc.name)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("diagnostic does not say %q; got:\n%v", want, err)
				}
			}
		})
	}
}

// TestVerifyNoticeAcceptsTheCuratedRecords is the other half of the mutation
// proof: every rule above must be satisfiable, and satisfied today by the four
// records as committed. Without it a rule that rejects everything — including
// correct input — would look identical to a rule that works.
func TestVerifyNoticeAcceptsTheCuratedRecords(t *testing.T) {
	classifier, err := lc.New(lc.DefaultConfidenceThreshold)
	if err != nil {
		t.Fatalf("build license classifier: %v", err)
	}
	for _, n := range nativeLibs {
		if err := n.verifyNotice(classifier); err != nil {
			t.Errorf("%v", err)
		}
	}
}

// TestVerifyNoticeFailureCarriesTheRemedy pins the tail of the diagnostic,
// because the remedy is doing work the rule cannot: this gate fires almost only
// on a version bump, at the exact moment the cheapest-looking fix is to
// reconcile the numbers and move on — which would discard the only signal that
// the text changed at all.
func TestVerifyNoticeFailureCarriesTheRemedy(t *testing.T) {
	classifier, err := lc.New(lc.DefaultConfidenceThreshold)
	if err != nil {
		t.Fatalf("build license classifier: %v", err)
	}
	lying := nativeLibNamed(t, "zstd")
	lying.license = "BUSL-1.1"
	err = lying.verifyNotice(classifier)
	if err == nil {
		t.Fatal("verifyNotice accepted a relicensed record")
	}
	if !strings.Contains(err.Error(), nativeNoticeRemedy) {
		t.Errorf("diagnostic omits the remedy; got:\n%v", err)
	}
}

// TestBuildEntriesRefusesALyingNativeRecord is the wiring proof. The rule is
// only worth having if the GATE runs it: verifyNotice passing in a unit test
// while `go run ./tools/licenses -check` never calls it would leave the four
// literals exactly as unchecked as before. It swaps the package record for a
// lying one and drives the real inventory build, which is the single function
// both -check and artifact generation go through.
func TestBuildEntriesRefusesALyingNativeRecord(t *testing.T) {
	original := nativeLibs
	t.Cleanup(func() { nativeLibs = original })

	lying := nativeLibNamed(t, "zstd")
	lying.license = "BUSL-1.1"
	nativeLibs = []nativeLib{lying}

	entries, err := buildEntries(litPkg)
	if err == nil {
		t.Fatalf("buildEntries returned %d entries for a native record the notice text contradicts", len(entries))
	}
	if !strings.Contains(err.Error(), `nothing corroborates its "BUSL-1.1"`) {
		t.Errorf("buildEntries failed for the wrong reason: %v", err)
	}
}
