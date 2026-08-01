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
	for _, want := range []string{"UNICODE LICENSE V3", "Zstandard", "MIT license"} {
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
	for _, tc := range []struct{ name, version, license string }{
		{"icu", "75.1", "Unicode-3.0"},
		{"zstd", "1.5.6", "BSD-3-Clause"},
		{"musl", "1.2.5", "MIT"},
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
		if len(c.Licenses) != 1 || c.Licenses[0].License.Name != tc.license {
			t.Errorf("%s licenses = %+v, want name %s", tc.name, c.Licenses, tc.license)
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
