package version

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoFile reads a path relative to the repository root. Tests run with cwd set
// to the package directory, so the root is two levels up.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestEveryProducerStampsOrigin is the machine-checked half of the contract this
// package's doc comment states in prose: the link-time variables have exactly
// three writers, and every one of them stamps every variable a consumer reads.
//
// It exists because the alternative — remembering — already failed once.
// FromSource was inferred from whether a Version was stamped, which made
// scripts/install.sh's `git describe` Version silently reclassify every
// locally-installed binary as a release; the surfaces that key on provenance
// went quiet on the only binary anyone runs, and every test still passed. A
// producer that forgets Origin recreates exactly that: a binary that presents
// itself as a release because nothing said otherwise. That failure is invisible
// from inside Go, so it is checked here, against the producers themselves.
func TestEveryProducerStampsOrigin(t *testing.T) {
	t.Parallel()

	// The two from-source entrypoints do not spell the value themselves; they
	// carry it from the one shared script, which is what keeps them from
	// drifting apart the way an inline copy in each could.
	ldflags := repoFile(t, "scripts/version-ldflags.sh")
	if !strings.Contains(ldflags, `LIT_BUILD_ORIGIN="`+OriginSource+`"`) {
		t.Errorf("scripts/version-ldflags.sh does not set LIT_BUILD_ORIGIN to %q", OriginSource)
	}
	if !strings.Contains(ldflags, "export") || !strings.Contains(ldflags, "LIT_BUILD_ORIGIN") {
		t.Error("scripts/version-ldflags.sh does not export LIT_BUILD_ORIGIN, so neither caller would receive it")
	}

	for _, site := range []struct{ file, want string }{
		{"Justfile", "${pkg}.Origin=${LIT_BUILD_ORIGIN}"},
		{"scripts/install.sh", "${pkg}.Origin=${LIT_BUILD_ORIGIN}"},
	} {
		if got := repoFile(t, site.file); !strings.Contains(got, site.want) {
			t.Errorf("%s does not pass %s to the linker — a binary it builds would carry no provenance and read as a release", site.file, site.want)
		}
	}

	// goreleaser is the only producer allowed to stamp "release", and it states
	// the value literally rather than from a template: the fact recorded is
	// about the producer, not about any particular build of it.
	const releaseStamp = "internal/version.Origin=" + OriginRelease
	if got := repoFile(t, ".goreleaser.yml"); !strings.Contains(got, releaseStamp) {
		t.Errorf(".goreleaser.yml does not stamp %s — released binaries would read as source builds and warn about their own age", releaseStamp)
	}
}

// TestOnlyGoreleaserClaimsReleaseProvenance pins the asymmetry that makes the
// default safe. Exactly one producer may say "release"; everything else — the
// from-source entrypoints, and any build that stamps nothing at all — must land
// on the side that reports its age, because an unknown producer is a binary
// nobody can vouch for and silence is the wrong answer for it.
func TestOnlyGoreleaserClaimsReleaseProvenance(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{"Justfile", "scripts/install.sh", "scripts/version-ldflags.sh"} {
		if strings.Contains(repoFile(t, rel), "Origin="+OriginRelease) {
			t.Errorf("%s stamps Origin=%s — only goreleaser may claim release provenance", rel, OriginRelease)
		}
	}
}
