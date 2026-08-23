package main

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	lc "github.com/google/licenseclassifier"
	packageurl "github.com/package-url/packageurl-go"
)

// Native C libraries that cgo statically links into lit's release binaries but
// that no go.mod-based tool can see: `go list -deps` reports only Go modules, so
// ICU, zstd, musl, and compiler-rt would be silently absent from the SBOM, the
// attribution bundle, the report, and the policy gate without this curated
// inventory. Their versions are pinned by the release build config, NOT
// discovered — see each entry's provenance comment; TestNativeICUVersionMatchesDockerfile
// checks the one version that lives in-repo (ICU, in build/Dockerfile.release)
// against drift.

// License texts are embedded because, unlike Go modules, there is no module
// directory to read them from at generation time. Each is the verbatim upstream
// notice, fetched from the pinned upstream tag; compiler-rt's file concatenates
// two such notices (Zig's LICENSE and LLVM compiler-rt's LICENSE.TXT) because
// the component genuinely carries material under both — see its entry below.
// [LAW:one-source-of-truth] the embedded bytes are the one copy that ships in
// THIRD_PARTY_LICENSES.
//
//go:embed native/ICU-LICENSE
var icuLicenseText string

//go:embed native/zstd-LICENSE
var zstdLicenseText string

//go:embed native/musl-COPYRIGHT
var muslLicenseText string

//go:embed native/compiler-rt-LICENSE
var compilerRTLicenseText string

// noticeRole says what one license identifier found in a native library's
// embedded notice text IS, relative to the license lit distributes that library
// under. It is a type rather than a remark because the three roles carry
// genuinely different rules: the same GPL-3.0 identifier is a build-stopping
// problem as roleBundled and an expected, disclosed fact as roleUnlinked, and
// only roleGranted may name an arm of the curated expression.
// [LAW:types-are-the-program]
type noticeRole int

const (
	// roleGranted is an AND-arm of the curated license: a grant lit
	// distributes the library under. Every such finding must be an arm, and
	// every arm the corpus can see must have one.
	roleGranted noticeRole = iota
	// roleBundled is third-party material folded INTO the shipped library
	// under its own permissive terms — ICU compiling Google's break-dictionary
	// data into its data blob. It is in the binary, so it is vetted: a
	// copyleft identifier may not wear this role.
	roleBundled
	// roleUnlinked is material the notice file carries that is not in the
	// linked library at all: upstream's own build scripts, which is the whole
	// of it today. It is exempt from the copyleft vet, and that exemption is
	// the entire content of the role — which is why a finding claiming it must
	// still name the material in covers, and why the role appears in the diff
	// as its own word rather than as silence.
	roleUnlinked
)

// noticeFinding is one license identifier the pinned classifier corpus finds in
// a native library's embedded notice text, how many times it occurs, and what
// that occurrence licenses.
//
// count is pinned rather than mere presence because the failure this record
// exists to catch is a version bump that changes the text while the curated
// license string stays put. ICU 76 folding in one more third-party break
// dictionary leaves the identifier SET identical and the count different, so a
// set-valued record would wave it through in silence — where a count forces the
// maintainer to look at what arrived. [LAW:no-silent-failure]
//
// Offsets are deliberately not pinned, because a reported offset is not a fact
// about the text: the classifier's offsets shift with the size of the window it
// is handed. Measured against this very file, ICU's two GPL-3.0 hits report at
// 19001 and 20100 over the whole notice and at 22181 and 23212 over the region
// that actually contains them, where the aclocal.m4 block genuinely begins at
// 22181. A record built on offsets would encode the window, not the notice.
type noticeFinding struct {
	identifier string
	count      int
	role       noticeRole
	// covers names the material these occurrences license, in upstream's own
	// terms. Required of every finding rather than only of the ones that need
	// defending: a reviewer reading the record then sees what each row IS
	// without cross-referencing the note, and a row nobody can describe is a
	// row nobody has checked. Every claim written here was attributed by
	// classifying the named section's bytes in isolation and confirming the
	// per-section totals reproduce the whole-file totals — never by finding
	// the identifier somewhere near a heading.
	covers string
}

// armEvidence corroborates a half of a curated AND-arm that the pinned corpus
// cannot speak about, by requiring a literal from that grant's own text to
// appear in the embedded bytes.
//
// Two halves need it, and both were measured rather than anticipated. An
// identifier absent from the 2021 corpus: ICU's Unicode-3.0, whose grant is the
// first 4147 bytes of ICU-LICENSE and matches nothing at all — classify that
// head region alone and the classifier returns zero matches. So the whole
// file's best match is its first BUNDLED notice, and a check comparing the
// classifier's verdict against the curated string would read ICU as
// BSD-3-Clause and fail a correct row forever. And every WITH-exception: the
// corpus names bases, never exceptions, so Apache-2.0 turning up in
// compiler-rt's notice says nothing about whether the LLVM exception beside it
// survived a version bump — the LLVM Exceptions section classifies as nothing,
// measured the same way.
//
// The two mechanisms partition the arms: an arm half is corroborated by a
// roleGranted finding or by evidence, never both and never neither. Redundant
// evidence is refused rather than tolerated, so a corpus that later learns
// Unicode-3.0 fails here and gets promoted to a finding, instead of leaving a
// second and weaker map of the same fact standing beside the first.
// [LAW:one-source-of-truth]
type armEvidence struct {
	token  string
	phrase string
}

// nativeLib is one statically-linked native C library: its name, version, a
// one-line statement of what it IS, the SPDX expression we distribute it under,
// the verbatim notice text, and a note carrying whatever a reader of the shipped
// artifacts needs to accept the license claim without consulting our source — a
// dual-license election, or the provenance behind a compound expression.
//
// description and note are two different kinds of claim and are kept apart
// deliberately. A Go module's name is a resolvable coordinate that identifies
// it; "icu" and "musl" are bare strings, so a reader of the SBOM alone has
// nothing telling them what those components are — which is what description
// answers. The note answers a different question, about the licence, and rides
// as a namespaced property (licenseNoteProperty) rather than in description,
// where CycloneDX's own definition does not fit it.
//
// They also ship to different places, which is worth knowing before shortening
// either. The note travels into all three artifacts — LICENSE-REPORT.md's Notes
// section, THIRD_PARTY_LICENSES, and the SBOM property — and is never only a Go
// comment. The description reaches the SBOM alone (see Entry.Description); no
// renderer of the report or the bundle reads it, because those two identify a
// component by the header they already print.
type nativeLib struct {
	name        string
	version     string
	description string
	license     string
	text        string
	note        string
	// acknowledgement is acknowledgementConcluded for a lib whose license row
	// lit arrived at rather than read off the notice — see Entry.Acknowledgement.
	acknowledgement string
	// findings and evidence together are what makes license a claim the build
	// checks rather than a claim the build repeats: findings is every license
	// identifier the pinned corpus finds in text, exhaustively, and evidence
	// corroborates the arms of license that the corpus cannot see. verifyNotice
	// reconciles all three against each other and against the bytes.
	findings []noticeFinding
	evidence []armEvidence
}

// nativeLibs is the curated native-library inventory. Versions are pinned to the
// release toolchain: ICU to build/Dockerfile.release's ICU_MAJOR.ICU_MINOR;
// zstd to the version gozstd vendors (its zstd.h ZSTD_VERSION_* — 1.5.6);
// musl and compiler-rt to what zig 0.14.0 (Dockerfile ZIG_VERSION) bundles for
// the fully-static linux/musl targets. ICU 75.1 carries the Unicode License v3
// (its LICENSE at release-75-1 is byte-identical to the embedded copy), and
// zig 0.14.0's bundled musl source identifies itself as 1.2.5
// (src/internal/version.h) with a COPYRIGHT byte-identical to ours.
//
// compiler-rt here is not LLVM's compiler-rt: it is Zig's own implementation of
// the builtins (lib/compiler_rt/*.zig in the zig tarball), MIT-licensed as part
// of Zig — but its files cite routines ported from llvm-project compiler-rt
// sources whose headers at the cited commits read "Apache-2.0 WITH
// LLVM-exception" (fp_add_impl.inc@02d85149, divdf3.c/divsf3.c@d674d96b,
// clear_cache.c@cf54cae2, os_version_check.c@llvmorg-13.0.0; others cite the
// pre-relicense era, when compiler_rt was dual licensed under the University of
// Illinois/NCSA license and MIT with the choice left to the user — lit elects
// MIT for those, which is why the expression carries two arms and not an NCSA
// third). The compound expression names both grants lit actually distributes
// under; flatly asserting MIT alone would misstate the Apache-licensed ported
// material, and adding NCSA would claim a grant lit declined.
//
// ICU and musl each bundle third-party material too, and each KEEPS its single
// identifier — the asymmetry with compiler-rt is deliberate, so don't "fix" it
// into a compound. Zig's LICENSE claims MIT over Zig's own code and is silent
// on the LLVM-derived routines, whose real grant (Apache-2.0 WITH
// LLVM-exception) carries patent and notice terms MIT does not: two materially
// different licenses, so the expression names both. ICU's and musl's notices
// are upstream's own grant for the WHOLE work, and the components they fold in
// are notice-only permissive ones imposing the same kind of obligation the
// stated license already does. Every license database resolves musl to MIT and
// ICU 75.1 to Unicode-3.0; emitting a compound here would make lit's SBOM
// disagree with what any scanner independently resolves for the coordinate,
// which is the contradiction this epic refused when it rejected a `replace`
// shim. What those components need is disclosure, not a different expression —
// so they carry notes, and the notes ship in all three artifacts.
var nativeLibs = []nativeLib{
	{name: "icu", version: "75.1", license: "Unicode-3.0", text: icuLicenseText,
		description: "International Components for Unicode: the C/C++ libraries providing Unicode text handling, collation, and regular-expression support.",
		note:        "ICU 75.1 is Unicode-3.0; its LICENSE at release-75-1 is byte-identical to the embedded copy. That file's Third-Party Software Licenses section carries further notices for components included within the ICU libraries — the pre-57 ICU License and the BSD-licensed cjdict.txt word-break data among them — and the embedded copy reproduces each verbatim. The two GNU GPL blocks the same file lists cover aclocal.m4 and config.guess, and the install-sh block beside them is MIT/X11: autotools build scripts, not code in any linked library.",
		// ICU is the reason evidence exists. Its own grant occupies the first
		// 44 lines and classifies as nothing — the 2021 corpus has no Unicode
		// license at any spelling, and LicenseType("Unicode-3.0") is "" too, so
		// neither the classifier nor the copyleft veto has one word to say
		// about the license ICU actually ships under. Everything the classifier
		// DOES find here is somebody else's notice.
		evidence: []armEvidence{{token: "Unicode-3.0", phrase: "UNICODE LICENSE V3"}},
		// Two of ICU's sections classify as nothing and so appear in neither
		// list: the pre-57 "ICU License - ICU 1.8.1 to ICU 57.1" and the Time
		// Zone Database's public-domain dedication. The record can only speak
		// about what the corpus can match, which is why the note carries the
		// rest in prose.
		findings: []noticeFinding{
			{identifier: "BSD-3-Clause", count: 5, role: roleBundled,
				covers: "the cjdict.txt Chinese/Japanese break dictionary, which carries three such notices (Google Inc., the TaBE Project, and the Computer Systems and Communication Lab at Academia Sinica); the burmesedict.txt break dictionary; and the bundled Google double-conversion sources — data and code ICU compiles into its libraries and its data blob"},
			{identifier: "BSD-2-Clause", count: 1, role: roleBundled,
				covers: "the laodict.txt break dictionary, whose section carries exactly one redistribution notice"},
			{identifier: "BSD-2-Clause-NetBSD", count: 1, role: roleBundled,
				covers: "that same single laodict.txt notice, which the classifier matches twice — once as BSD-2-Clause and once as its NetBSD variant — rather than a second body of material"},
			// The one place a copyleft identifier is allowed to stand, and the
			// attribution is by bytes: classifying lines 435-470 and 470-500 in
			// isolation returns exactly one GPL-3.0 each, and classifying
			// everything before line 435 returns none at all. So these two hits
			// ARE the autotools blocks rather than merely near them.
			{identifier: "GPL-3.0", count: 2, role: roleUnlinked,
				covers: "the aclocal.m4 and config.guess autotools scripts, which ICU4C's own build runs and which are not compiled into any library lit links"},
		}},
	{name: "zstd", version: "1.5.6", license: "BSD-3-Clause", text: zstdLicenseText,
		description:     "Zstandard: a fast lossless data-compression library.",
		acknowledgement: acknowledgementConcluded,
		note:            "zstd is dual-licensed upstream (BSD-3-Clause OR GPL-2.0-only) at the user's election; lit elects and distributes under BSD-3-Clause.",
		// The elected arm is the only license in these bytes — the un-elected
		// GPL-2.0 alternative the note names is stated elsewhere by upstream
		// and appears nowhere in the embedded text, which carries no mention of
		// the GPL at all. So a change of election would arrive here as this
		// finding's disappearance, not as a second finding beside it.
		findings: []noticeFinding{
			{identifier: "BSD-3-Clause", count: 1, role: roleGranted,
				covers: "zstd itself: the arm lit elects out of upstream's BSD-3-Clause OR GPL-2.0-only dual grant"},
		}},
	{name: "musl", version: "1.2.5", license: "MIT", text: muslLicenseText,
		// No platform clause. The curated inventory is generated ONCE per
		// release and rendered into artifacts that are not platform-specific:
		// LICENSE-REPORT.md ships inside every archive .goreleaser.yml builds —
		// darwin and windows included — under a preamble asserting the
		// components listed are compiled into this binary, and the SBOM ships
		// as a single standalone asset covering all of them. A description
		// reading "linked into lit's Linux builds" is therefore a false
		// sentence in the copies a darwin or windows recipient opens.
		description: "musl libc: a lightweight implementation of the C standard library.",
		note:        "musl's COPYRIGHT is upstream's own grant for the library as a whole: MIT, with portions derived from third-party works under terms upstream states are compatible with that MIT license — the TRE regular-expression code (2-clause BSD), the ARM and AArch64 memcpy/memset routines, and David Burren's DES implementation (BSD) among them. What ships is that COPYRIGHT verbatim: upstream's own attribution document, a component-by-component list of who holds each copyright, and not a reproduction of the third-party license texts — for TRE it says outright that the text lives in the source files.",
		// One finding, and the note explains why it is one: this document
		// ATTRIBUTES the third-party works rather than reproducing their
		// license texts, so there is no second grant in these bytes for the
		// classifier to find. That is a real limit on what this record proves
		// for musl, and it is the note, not the record, that carries the
		// component-by-component picture.
		findings: []noticeFinding{
			{identifier: "MIT", count: 1, role: roleGranted,
				covers: "musl as a whole: upstream's own grant, stated once at the head of COPYRIGHT"},
		}},
	{name: "compiler-rt", version: "0.14.0", license: "MIT AND Apache-2.0 WITH LLVM-exception", text: compilerRTLicenseText,
		description:     "Zig's implementation of the compiler-rt builtins: the low-level runtime routines a compiler emits calls to, such as integer division and floating-point arithmetic on targets lacking hardware support.",
		acknowledgement: acknowledgementConcluded,
		note:            "Zig 0.14.0's implementation of the compiler-rt builtins, versioned by the Zig toolchain that bundles it: MIT as part of Zig, and it includes routines ported from LLVM compiler-rt sources licensed Apache-2.0 WITH LLVM-exception. Routines ported from pre-relicense LLVM sources are dual-licensed (University of Illinois/NCSA OR MIT) at the user's election; lit elects and distributes those under MIT, which the expression's MIT arm covers, so no NCSA grant is claimed. The embedded notice carries the full text of every license named here.",
		// The compound expression the epic settled on, checked arm by arm. Its
		// Apache arm needs evidence for the reason every WITH-exception does:
		// the LLVM Exceptions section classifies as nothing on its own, so
		// dropping it upstream would leave both Apache-2.0 findings intact and
		// this record unmoved.
		evidence: []armEvidence{
			{token: "LLVM-exception", phrase: "---- LLVM Exceptions to the Apache 2.0 License ----"},
		},
		// The NCSA grant this file also carries is absent below, and that is
		// the classifier's silence rather than the file's: the University of
		// Illinois/NCSA section classifies as nothing even though the corpus
		// has an NCSA entry (LicenseType("NCSA") is "notice"). Nothing is lost
		// by that here, since lit declines the NCSA arm and the note says so —
		// but read the record as what the corpus can see, not as the whole
		// contents of the notice.
		findings: []noticeFinding{
			{identifier: "MIT", count: 2, role: roleGranted,
				covers: "Zig's own grant over lib/compiler_rt, and — separately — the MIT arm of the legacy LLVM dual license, which lit elects for the routines ported from pre-relicense sources"},
			{identifier: "Apache-2.0", count: 2, role: roleGranted,
				covers: "the LLVM grant over the routines Zig ported from llvm-project, counted twice because that license's own APPENDIX restates it as a header for attaching to a work"},
		}},
}

// nativeNoticeRemedy tails every verification failure. It names re-reading the
// notice FIRST, and deliberately does not offer "adjust the count until it
// passes" as a step: this gate fires almost exclusively on a version bump, at
// the one moment when the cheapest-looking fix — reconcile the numbers, ship —
// is also the one that discards the only signal that anything changed.
const nativeNoticeRemedy = "Resolve by reading the embedded notice and saying what it now grants: correct the license expression if the grant changed, correct the findings if the bundled material changed, and correct the note and THIRD_PARTY_LICENSES with it. Reconciling the record to the classifier without reading the text defeats the check — the numbers are how the text speaks, not the thing being checked."

// nativeEntries renders the curated native libraries as Entry values so the
// bundle, report, SBOM, and policy gate all treat them exactly like Go modules —
// same []Entry, one inventory. The purl is pkg:generic (not pkg:golang): these
// are not Go modules, and a pkg:golang purl would misdirect a scanner to the Go
// module proxy. [LAW:one-type-per-behavior] a native lib is a component like any
// other; only its purl namespace differs.
//
// It takes the classifier and returns an error because the curated license
// literals are verified against the embedded notice bytes on the way out, and
// the only way to obtain a native Entry is to have passed that verification.
// [LAW:parse-dont-validate] the []Entry is the stamped type: no consumer
// re-checks the literals, and no consumer can skip the check by reaching for
// nativeLibs directly and rendering it itself.
func nativeEntries(classifier *lc.License) ([]Entry, error) {
	entries := make([]Entry, 0, len(nativeLibs))
	for _, n := range nativeLibs {
		if err := n.verifyNotice(classifier); err != nil {
			return nil, err
		}
		entries = append(entries, Entry{
			Module:          Module{Path: n.name, Version: n.version},
			LicenseName:     n.license,
			Text:            n.text,
			Description:     n.description,
			Note:            n.note,
			Acknowledgement: n.acknowledgement,
			PackageURL:      genericPURL(n.name, n.version),
		})
	}
	return entries, nil
}

// verifyNotice checks one native library's curated license record against what
// the pinned classifier corpus actually finds in its embedded notice bytes, and
// reports every disagreement at once.
//
// This is what makes "permissive only" mean something for the half of the
// inventory that never passes through Classify. The copyleft veto and the
// hard failure on the Unknown sentinel both sit downstream of a classifier
// verdict, so before this a native license was an exact string with nothing
// behind it: a maintainer bumping a version could write any identifier at all,
// add it to allowed_licenses, and -check would print green with nothing in the
// tree to say otherwise. [LAW:no-silent-failure]
//
// The check is a reconciliation, not a comparison, because a comparison is
// false here. These are notice DOCUMENTS, not single-license files: measured,
// ICU's carries eight matchable notices and its own grant matches nothing,
// while compiler-rt's carries four. So "the classifier's verdict equals the
// curated string" would fail two of the four correct rows today. What holds
// instead is that every identifier in the bytes has a declared disposition and
// every arm of the curated expression has corroboration — an equality in both
// directions, so the record can rot neither by the text gaining a license nor
// by it losing one.
func (n nativeLib) verifyNotice(classifier *lc.License) error {
	arms, err := parseLicenseExpression(fmt.Sprintf("native library %s's license", n.name), n.license)
	if err != nil {
		return err
	}

	// Headers matter here in a way they do not for a Go module's LICENSE file,
	// which is why this does not go through Classify. Copyleft reaches a source
	// tree overwhelmingly as a NOTICE HEADER — "This program is free software;
	// you can redistribute it..." — not as the full GPL text, and MultipleMatch
	// skips header forms unless asked. Measured on ICU: with headers off the
	// classifier reports zero GPL in a notice that plainly contains two GPL
	// blocks. A check blind to exactly the form copyleft usually arrives in
	// would be the wrong check.
	found := map[string]int{}
	for _, m := range classifier.MultipleMatch(n.text, true) {
		found[m.Name]++
	}

	var problems []string
	note := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	armBases := map[string]bool{}
	for _, a := range arms {
		armBases[a.base] = true
	}

	declared := make(map[string]noticeFinding, len(n.findings))
	granted := map[string]bool{}
	for _, f := range n.findings {
		if _, dup := declared[f.identifier]; dup {
			note("declares %q twice; one row per identifier, carrying the total count", f.identifier)
			continue
		}
		declared[f.identifier] = f
		if f.count < 1 {
			note("declares %q with a count of %d; a finding records occurrences the classifier found, so its count is at least one", f.identifier, f.count)
		}
		if strings.TrimSpace(f.covers) == "" {
			note("declares %q without naming the material it licenses; every finding carries covers, including the ones that look obvious", f.identifier)
		}
		switch f.role {
		case roleGranted:
			granted[f.identifier] = true
			if !armBases[f.identifier] {
				note("declares %q as a grant lit distributes under, but it is not an arm of the curated license %q; either the expression is missing an arm or this is bundled material", f.identifier, n.license)
			}
		case roleBundled:
			if armBases[f.identifier] {
				note("declares %q as bundled third-party material while the curated license %q grants under it; an arm of the expression is roleGranted", f.identifier, n.license)
			}
			// The one veto this record enforces, and it applies here and not
			// to roleUnlinked because the roles differ in exactly this: bundled
			// material is compiled INTO the shipped binary.
			if kind, spelling := copyleftType(f.identifier); kind != "" {
				note("declares %q as material bundled into the shipped library, but the classifier types it %q%s; copyleft compiled into the binary is a component to remove, not a row this record can absorb", f.identifier, kind, copyleftVia(f.identifier, spelling))
			}
		case roleUnlinked:
			if armBases[f.identifier] {
				note("declares %q as material outside the linked library while the curated license %q grants under it", f.identifier, n.license)
			}
		default:
			note("declares %q with role %d, which verifyNotice does not know how to rule on; teach it the new role before shipping one", f.identifier, f.role)
		}
	}

	// Both directions, so the record cannot rot either way: an identifier that
	// arrived in the text is undeclared, and one that left it is stale.
	// [LAW:one-source-of-truth]
	for _, id := range sortedKeys(found) {
		f, ok := declared[id]
		if !ok {
			note("the classifier finds %q in the embedded notice text %s, and this record says nothing about it — a license arrived that nobody has looked at", id, occurrences(found[id]))
			continue
		}
		if f.count != found[id] {
			note("declares %q %s, but the classifier finds it %s in the embedded notice text", id, occurrences(f.count), occurrences(found[id]))
		}
	}
	for _, id := range sortedKeys(declared) {
		if found[id] == 0 {
			note("declares %q, which the classifier no longer finds in the embedded notice text at all", id)
		}
	}

	evidence := make(map[string]string, len(n.evidence))
	for _, e := range n.evidence {
		if _, dup := evidence[e.token]; dup {
			note("carries two pieces of evidence for %q", e.token)
			continue
		}
		evidence[e.token] = e.phrase
	}

	// needsEvidence is every arm half the corpus cannot reach: a base that no
	// roleGranted finding names, plus every exception there is. Naming the set
	// first and then comparing it to the evidence in both directions is what
	// makes the two corroboration mechanisms PARTITION the arms — each half
	// vouched for exactly once, never twice and never not at all.
	needsEvidence := map[string]string{} // token -> the arm it is half of
	for _, a := range arms {
		if !granted[a.base] {
			needsEvidence[a.base] = armText(a)
		}
		if a.exception != "" {
			needsEvidence[a.exception] = armText(a)
		}
	}
	for _, token := range sortedKeys(needsEvidence) {
		phrase, ok := evidence[token]
		if !ok {
			note("the curated license %q grants under %q, and nothing corroborates its %q: no roleGranted finding names it, and no armEvidence quotes it. The corpus names bases and never exceptions, and for some identifiers lit legitimately ships it has no entry at all — corroborate it with a literal from that grant's own text", n.license, needsEvidence[token], token)
			continue
		}
		if !strings.Contains(n.text, phrase) {
			note("the evidence for %q quotes %q, which does not appear in the embedded notice text; that phrase is the whole of what corroborates a grant the classifier cannot see, so a phrase that is not there corroborates nothing", token, phrase)
		}
	}
	// The other direction. Presence is read with the two-value form rather than
	// by comparing the value to "": the map's values are display strings, and a
	// lookup that cannot tell "absent" from "present and empty" is the shape
	// that turns a missing key into a silent pass.
	for _, token := range sortedKeys(evidence) {
		if _, needed := needsEvidence[token]; needed {
			continue
		}
		if granted[token] {
			note("carries evidence for %q, which a roleGranted finding already corroborates; the classifier having reached this identifier makes the phrase a second and weaker record of the same fact — drop the evidence and keep the finding", token)
			continue
		}
		note("carries evidence for %q, which is not a half of any arm of the curated license %q", token, n.license)
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("native library %s: the embedded notice text and native.go's curated license record disagree:\n  - %s\n%s",
		n.name, strings.Join(problems, "\n  - "), nativeNoticeRemedy)
}

// sortedKeys returns m's keys in lexical order, so a record with several
// disagreements reports them in the same order on every run.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// occurrences renders a count the way the diagnostics read.
func occurrences(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

// genericPURL builds a pkg:generic Package URL for a non-Go component.
func genericPURL(name, version string) string {
	return packageurl.NewPackageURL(packageurl.TypeGeneric, "", name, version, nil, "").ToString()
}
