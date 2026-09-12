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

// exportedNames returns the variable names carried on a shell script's `export`
// statements.
//
// The membership question has to be asked of the statement, never of the file.
// scripts/version-ldflags.sh contains the word "export" and the word
// "LIT_BUILD_ORIGIN" whichever way it is written — the first on the line
// exporting Commit and Date, the second on its own assignment — so a pair of
// substring tests over the file body is true even when the export line has
// dropped the variable entirely, which is precisely the regression worth
// catching. Parsing returns the set the caller actually needs, leaving no way
// to satisfy the check except by exporting. [LAW:parse-dont-validate]
//
// Membership rather than a match on the literal `export A B C` line, because
// the contract is that the variable leaves the script, not the order the three
// names are written in. [LAW:behavior-not-structure]
func exportedNames(script string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(script, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "export" {
			continue
		}
		// `export FOO=bar` names FOO just as `export FOO` does.
		for _, field := range fields[1:] {
			names[strings.SplitN(field, "=", 2)[0]] = true
		}
	}
	return names
}

// TestEveryProducerStampsOrigin is the machine-checked half of the contract the
// var block above declines to recite: the link-time variables have exactly
// three writers, and every one of them stamps Origin. Version is the single
// field a writer may withhold, and the asymmetry is
// TestOnlyTheJustfileOmitsVersion's to own.
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
	exported := exportedNames(ldflags)
	for _, name := range []string{"LIT_BUILD_COMMIT", "LIT_BUILD_DATE", "LIT_BUILD_ORIGIN"} {
		if !exported[name] {
			t.Errorf("scripts/version-ldflags.sh assigns but never exports %s, so neither caller would receive it", name)
		}
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

// TestOnlyTheJustfileOmitsVersion pins the omission the IsDev discriminator is
// built on, and it is the half the doc comment above the var block used to
// assert in prose and nothing checked. `just build` deliberately does not stamp
// Version; that is what leaves internal/version.Version empty, and so IsDev
// true, which internal/store/migration_runner.go's producer-binary-version
// guard relies on to keep an ordinary local build from overwriting a real
// release's downgrade stamp. The other two producers must stamp it —
// scripts/install.sh from `git describe`, goreleaser from the tag — each
// opting its own binary out of that guard on purpose.
//
// Both halves are asserted because only one of them is the interesting failure
// today and either is fatal. An edit adding -X .../version.Version=… to the
// build recipe would flip every `just build` binary out of dev mode; an edit
// dropping it from install.sh would put every installed binary INTO dev mode
// and back under a guard it is meant to be exempt from. A test that checked
// only the omission would pass through the second one unchanged.
func TestOnlyTheJustfileOmitsVersion(t *testing.T) {
	t.Parallel()

	// Matched in both spellings a producer could use: the recipes build the
	// flag from a ${pkg} variable, goreleaser writes the import path out.
	justfile := repoFile(t, "Justfile")
	for _, form := range []string{"${pkg}.Version=", "internal/version.Version="} {
		if strings.Contains(justfile, form) {
			t.Errorf("the Justfile stamps %s — `just build` would stop being IsDev, and the migration runner's producer guard would treat a local build as a release", form)
		}
	}

	for _, site := range []struct{ file, want string }{
		{"scripts/install.sh", "${pkg}.Version="},
		{".goreleaser.yml", "internal/version.Version="},
	} {
		if !strings.Contains(repoFile(t, site.file), site.want) {
			t.Errorf("%s no longer stamps %s — the binaries it produces would read as dev builds and fall back under the downgrade guard they are meant to be exempt from", site.file, site.want)
		}
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
