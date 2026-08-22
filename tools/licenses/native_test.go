package main

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestNativeLibsInSBOMAndBundle is this ticket's acceptance criterion: the SBOM
// and the attribution bundle both list ICU, zstd, and musl (and compiler-rt)
// with a license and a version, and the bundle carries their notice text. It
// renders from nativeEntries() — the exact Entry values buildEntries appends to
// the Go inventory — so it checks precisely what the generated artifacts contain.
func TestNativeLibsInSBOMAndBundle(t *testing.T) {
	entries := nativeEntries()

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
		got := licenseChoice(tc.in)
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
// artifacts themselves — LICENSE-REPORT.md's Notes section and the SBOM
// component's description — not only in native.go, which ships to nobody.
func TestNativeNotesSurfaceInReportAndSBOM(t *testing.T) {
	entries := nativeEntries()

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
	descriptions := map[string]string{}
	for _, c := range bom.Components {
		descriptions[c.Name] = c.Description
	}
	if !strings.Contains(descriptions["zstd"], "BSD-3-Clause OR GPL-2.0-only") {
		t.Errorf("zstd SBOM description does not state the dual-license election: %q", descriptions["zstd"])
	}
	if !strings.Contains(descriptions["compiler-rt"], "Apache-2.0 WITH LLVM-exception") {
		t.Errorf("compiler-rt SBOM description does not state the ported-LLVM provenance: %q", descriptions["compiler-rt"])
	}
	// Libs without a curated note stay description-free — the note is curated
	// information, never boilerplate.
	for _, name := range []string{"icu", "musl"} {
		if descriptions[name] != "" {
			t.Errorf("%s has an unexpected SBOM description %q", name, descriptions[name])
		}
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
	if v := CheckPolicy(nativeEntries(), policy); len(v) != 0 {
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
