package version

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoFile reads a path relative to the repository root. Tests run with cwd set
// to the package directory, so the root is two levels up.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(repoPath(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// repoPath joins elems onto the repository root.
func repoPath(elems ...string) string {
	return filepath.Join(append([]string{"..", ".."}, elems...)...)
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
// A producer that forgets Origin makes a binary that presents itself as a
// release because nothing said otherwise: the surfaces that key on provenance go
// quiet on the only binary anyone runs, and every test still passes. That
// failure is invisible from inside Go, so it is checked here, against the
// producers themselves.
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
// built on. `just build` deliberately does not stamp Version; that is what
// leaves internal/version.Version empty, and so IsDev true: `lit version`
// reports the binary as a dev build, and `lit upgrade` never calls it already
// current. The other two producers must stamp it — scripts/install.sh from
// `git describe`, goreleaser from the tag — so their binaries carry a version
// `lit upgrade` can compare against the latest release.
//
// Both halves are asserted because only one of them is the interesting failure
// today and either is fatal. An edit adding -X .../version.Version=… to the
// build recipe would flip every `just build` binary out of dev mode; an edit
// dropping it from install.sh would put every installed binary INTO dev mode,
// where `lit upgrade` reinstalls over it on every run. A test that checked
// only the omission would pass through the second one unchanged.
func TestOnlyTheJustfileOmitsVersion(t *testing.T) {
	t.Parallel()

	// Matched in both spellings a producer could use: the recipes build the
	// flag from a ${pkg} variable, goreleaser writes the import path out.
	justfile := repoFile(t, "Justfile")
	for _, form := range []string{"${pkg}.Version=", "internal/version.Version="} {
		if strings.Contains(justfile, form) {
			t.Errorf("the Justfile stamps %s — `just build` would stop being IsDev, and `lit version` would stop reporting a local build as one", form)
		}
	}

	for _, site := range []struct{ file, want string }{
		{"scripts/install.sh", "${pkg}.Version="},
		{".goreleaser.yml", "internal/version.Version="},
	} {
		if !strings.Contains(repoFile(t, site.file), site.want) {
			t.Errorf("%s no longer stamps %s — the binaries it produces would read as dev builds, and `lit upgrade` would reinstall over them on every run", site.file, site.want)
		}
	}
}

// TestOnlyGoreleaserClaimsReleaseProvenance pins the asymmetry that makes the
// default safe. Exactly one producer may say "release"; everything else — the
// from-source entrypoints, and any build that stamps nothing at all — must land
// on the side that reports its age, because an unknown producer is a binary
// nobody can vouch for and silence is the wrong answer for it.
//
// The one exception is scripts/cross-version-check.sh: it builds a release
// stand-in into a temporary directory it removes on exit, to step a store down
// and raise it back as a release would. Every other build site in the Justfile,
// scripts/ and .github/ is scanned, so a further claimant fails here.
func TestOnlyGoreleaserClaimsReleaseProvenance(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{".goreleaser.yml": true, "scripts/cross-version-check.sh": true}
	sites := []string{"Justfile"}
	for _, dir := range []string{"scripts", ".github"} {
		err := filepath.WalkDir(repoPath(dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(repoPath(), path)
			sites = append(sites, filepath.ToSlash(rel))
			return err
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	for _, rel := range sites {
		if !allowed[rel] && strings.Contains(repoFile(t, rel), ".Origin="+OriginRelease) {
			t.Errorf("%s stamps Origin=%s — only goreleaser, and the cross-version check's throwaway build, may claim release provenance", rel, OriginRelease)
		}
	}
	if !strings.Contains(repoFile(t, "scripts/cross-version-check.sh"), ".Origin="+OriginRelease) {
		t.Error("scripts/cross-version-check.sh no longer stamps release provenance; drop its exception here")
	}
}

// TestEveryProducerStampsTheReleasedSchema pins the released-schema ceiling at
// every producer. A producer that forgets it builds a lit with ceiling 0, which
// migrates no existing workspace and pushes nothing; one that stamped the
// tree's own schema would let an unreleased migration reach a real workspace.
// The from-source producers carry the number the one shared script reads from
// the newest release tag; goreleaser states that a release's schema is its own.
func TestEveryProducerStampsTheReleasedSchema(t *testing.T) {
	t.Parallel()

	ldflags := repoFile(t, "scripts/version-ldflags.sh")
	if !strings.Contains(ldflags, "tools/schema-release-check -released-schema") {
		t.Error("scripts/version-ldflags.sh does not read LIT_BUILD_RELEASED_SCHEMA from tools/schema-release-check -released-schema")
	}
	if !strings.Contains(ldflags, `LIT_BUILD_RELEASED_SCHEMA="${_lit_released}@${_lit_newest_tag:-`+releasedLabelNoTag+`}"`) {
		t.Errorf("scripts/version-ldflags.sh does not stamp <version>@<tag>, or @%s with no tag — the ceiling's source would be lost", releasedLabelNoTag)
	}
	// A tree with no .git of its own reads no tags, as install.sh's guard
	// reads none, so a tarball inside another repo cannot borrow its tags.
	if !strings.Contains(ldflags, `if [ -e "$_lit_root/.git" ]; then`) {
		t.Error("scripts/version-ldflags.sh reads release tags without checking the checkout has its own .git")
	}
	if !exportedNames(ldflags)["LIT_BUILD_RELEASED_SCHEMA"] {
		t.Error("scripts/version-ldflags.sh never exports LIT_BUILD_RELEASED_SCHEMA, so neither caller would receive it")
	}
	for _, file := range []string{"Justfile", "scripts/install.sh"} {
		if !strings.Contains(repoFile(t, file), "${pkg}.ReleasedSchema=${LIT_BUILD_RELEASED_SCHEMA}") {
			t.Errorf("%s does not pass ${pkg}.ReleasedSchema=${LIT_BUILD_RELEASED_SCHEMA} to the linker — a lit it builds would have released-schema ceiling 0", file)
		}
	}
	// Only a real release claims its own schema; a snapshot is stamped 0.
	const releaseStamp = "internal/version.ReleasedSchema={{ if .IsSnapshot }}0@" + releasedLabelSnapshot + "{{ else }}" + ReleasedSchemaCarried + "{{ end }}"
	if !strings.Contains(repoFile(t, ".goreleaser.yml"), releaseStamp) {
		t.Errorf(".goreleaser.yml does not stamp %s — released binaries would refuse every migration and push, or snapshots would claim to be releases", releaseStamp)
	}
}

// TestNoCIJobWritesToTheTracker pins the owner's ruling that CI holds no
// write access to the lit backlog. The backlog is the refs/dolt/data ref of
// this repository, so any job holding a contents write grant can push it,
// whatever it runs: every workflow declares its top-level permissions, none of
// them writes contents, and the one job that may is release-validate.yml's
// publish job, which creates the release tag. A token from any other secret
// would need no grant, so no file under .github reads a secret but the
// workflow's own GITHUB_TOKEN, and none names the ways lit writes a remote
// store. A scheduled job that held the grant once migrated the shared store to
// a schema no release carries.
func TestNoCIJobWritesToTheTracker(t *testing.T) {
	t.Parallel()
	const publisherFile, publisherJob = "release-validate.yml", "publish"
	dir := repoPath(".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	type permissioned struct {
		Permissions any `yaml:"permissions"`
	}
	var workflow struct {
		Permissions any                     `yaml:"permissions"`
		Jobs        map[string]permissioned `yaml:"jobs"`
	}
	writesContents := func(p any) bool {
		switch v := p.(type) {
		case string:
			return v == "write-all"
		case map[string]any:
			return v["contents"] == "write"
		}
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !(strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		workflow.Permissions, workflow.Jobs = nil, nil
		if err := yaml.Unmarshal(b, &workflow); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if workflow.Permissions == nil {
			t.Errorf("%s declares no top-level permissions, so its jobs get the repository's default token", name)
		} else if writesContents(workflow.Permissions) {
			t.Errorf("%s grants contents write to every job", name)
		}
		for job, j := range workflow.Jobs {
			if writesContents(j.Permissions) && (name != publisherFile || job != publisherJob) {
				t.Errorf("%s job %s grants contents write — CI never writes to the lit tracker", name, job)
			}
		}
	}
	pushes := regexp.MustCompile(`sync\s+push|refs/dolt|report-to-lit`)
	secret := regexp.MustCompile(`secrets\.([A-Za-z0-9_]+)`)
	err = filepath.WalkDir(repoPath(".github"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if m := pushes.Find(b); m != nil {
			t.Errorf("%s contains %q — CI never writes to the lit tracker", path, m)
		}
		for _, m := range secret.FindAllSubmatch(b, -1) {
			if string(m[1]) != "GITHUB_TOKEN" {
				t.Errorf("%s reads secret %s — CI's only credential is its GITHUB_TOKEN, whose grant this test bounds", path, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSchemaCeilingReadsTheStamp pins the stamp's grammar and the
// source each form names: unstamped fails closed at 0, the release stamp is
// the carried schema, "<version>@<label>" is that version from the tag (or the
// snapshot, or no tag) the label names, and anything else is a broken
// producer.
func TestSchemaCeilingReadsTheStamp(t *testing.T) {
	saved := ReleasedSchema
	t.Cleanup(func() { ReleasedSchema = saved })
	carried, err := CarriedSchemaSupport()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		stamp  string
		want   int64
		source string
		ok     bool
	}{
		{"", 0, "without a released-schema stamp", true},
		{ReleasedSchemaCarried, carried.Max, "is a release", true},
		{"11@v0.24.0", 11, "release tag v0.24.0", true},
		{"0@none", 0, "had no release tag", true},
		{"0@snapshot", 0, "snapshot build", true},
		{"11", 0, "", false},
		{"11@", 0, "", false},
		{"-1@v0.1.0", 0, "", false},
		{"v11@v0.1.0", 0, "", false},
	} {
		ReleasedSchema = tc.stamp
		got, err := ReadReleasedSchemaCeiling()
		if (err == nil) != tc.ok || got.Max != tc.want || !strings.Contains(got.Source, tc.source) {
			t.Errorf("ReadReleasedSchemaCeiling() with stamp %q = %+v, %v; want max %d naming %q, ok=%v", tc.stamp, got, err, tc.want, tc.source, tc.ok)
		}
	}
}
