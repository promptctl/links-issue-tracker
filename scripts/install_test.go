//go:build unix

// Package scripts holds the behavioral tests of the shell scripts beside it.
package scripts

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	litrelease "github.com/promptctl/links-issue-tracker/internal/release"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"golang.org/x/mod/semver"
)

const (
	oldLit    = "#!/bin/sh\necho 'lit old'\n"
	newLit    = "#!/bin/sh\necho 'lit new'\n"
	brokenLit = "#!/bin/sh\necho 'boom' >&2\nexit 3\n"
)

// schemaReleaseCheck is the real tools/schema-release-check, built once from
// this tree. It embeds this tree's migration registry, so this tree's schema is
// the "tree" side of every fixture's comparison.
var schemaReleaseCheck string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "schema-release-check")
	if err != nil {
		panic(err)
	}
	schemaReleaseCheck = filepath.Join(dir, "schema-release-check")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", schemaReleaseCheck, "../tools/schema-release-check")
	if out, err := build.CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build schema-release-check: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// release is a tag in a fixture repo whose registry is this tree's migrations
// 1..schema. A diverged release holds different bytes for its highest one.
type release struct {
	tag      string
	schema   int64
	diverged bool
}

// treeSchema is the schema of the tree these tests run in.
func treeSchema(t *testing.T) int64 {
	t.Helper()
	v, err := migrations.MaxVersion()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// initFixtureRepo makes root a git repo carrying one commit and tag per
// release, each commit's internal/store/migrations holding this tree's
// migrations 1..schema.
func initFixtureRepo(t *testing.T, root string, releases []release) {
	t.Helper()
	const treeRegistry = "../internal/store/migrations"
	entries, err := os.ReadDir(treeRegistry)
	if err != nil {
		t.Fatal(err)
	}
	migrationFiles := map[int64]string{}
	for _, e := range entries {
		if v, ok := migrations.ParseVersion(e.Name()); ok && strings.HasSuffix(e.Name(), ".sql") {
			migrationFiles[v] = e.Name()
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "tag.gpgSign=false", "-c", "commit.gpgSign=false"}, args...)...)
		cmd.Dir = root
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	registry := filepath.Join(root, "internal", "store", "migrations")
	for _, r := range releases {
		if err := os.RemoveAll(registry); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(registry, 0o755); err != nil {
			t.Fatal(err)
		}
		for v := int64(1); v <= r.schema; v++ {
			content := readFile(t, filepath.Join(treeRegistry, migrationFiles[v]))
			if r.diverged && v == r.schema {
				content += "\n-- not this tree's bytes\n"
			}
			writeFile(t, filepath.Join(registry, migrationFiles[v]), content, 0o644)
		}
		writeFile(t, filepath.Join(registry, "README.md"), "", 0o644)
		git("add", "-A", "internal")
		git("commit", "-q", "--allow-empty", "-m", r.tag)
		git("tag", r.tag)
	}
}

// installRun is one source-mode run of install.sh over a target dir that
// already holds a working lit.
type installRun struct {
	target         string // the installed lit, symlinks resolved as install.sh resolves them
	oldIno         uint64
	built          []string // the packages install.sh asked go to build, in order
	stdout, stderr string
	err            error
}

// runSourceInstall runs the real install.sh, copied into checkout, a directory
// of a fixture repo carrying releases, in its default source mode. checkout ""
// is the repo itself; any other names a tree with no .git of its own inside the
// repo, as a source tarball unpacked there is. A stub `go` on PATH stands in for both
// builds: the schema checker gets the real one built in TestMain, and lit gets
// built, written in place to whatever path follows -o, so a build aimed at the
// installed file keeps that file's inode. The two helper scripts install.sh
// sources for the lit build are replaced by stubs; they set up a real Go build,
// and the build is not what these tests are about.
func runSourceInstall(t *testing.T, built string, releases []release, checkout string) installRun {
	t.Helper()
	root := t.TempDir()
	scriptDir := filepath.Join(root, checkout, "scripts")
	stubBin := filepath.Join(root, "stub-bin")
	targetDir := filepath.Join(root, "target-bin")
	for _, d := range []string{scriptDir, stubBin, targetDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initFixtureRepo(t, root, releases)
	for _, name := range []string{"install.sh", "release-tags.sh"} {
		writeFile(t, filepath.Join(scriptDir, name), readFile(t, name), 0o755)
	}
	writeFile(t, filepath.Join(scriptDir, "cgo-env.sh"), ":\n", 0o644)
	writeFile(t, filepath.Join(scriptDir, "version-ldflags.sh"),
		"export LIT_BUILD_COMMIT=test LIT_BUILD_DATE=test LIT_BUILD_ORIGIN=source\n", 0o644)
	buildLog := filepath.Join(root, "go-builds")
	writeFile(t, filepath.Join(stubBin, "go"), `#!/bin/sh
for a; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
echo "$prev" >> "$BUILD_LOG"
case "$prev" in
  ./tools/schema-release-check) /bin/cp -f "$SCHEMA_RELEASE_CHECK" "$out" ;;
  ./cmd/lit) printf '%s' "$FAKE_LIT" > "$out"; chmod +x "$out" ;;
  *) echo "stub go: unexpected package $prev" >&2; exit 1 ;;
esac
`, 0o755)
	writeFile(t, filepath.Join(targetDir, "lit"), oldLit, 0o755)

	// install.sh picks the dir of the lit already on PATH and canonicalizes it,
	// which on macOS turns /var into /private/var.
	resolvedDir, err := filepath.EvalSymlinks(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	run := installRun{target: filepath.Join(resolvedDir, "lit")}
	run.oldIno = inode(t, run.target)

	cmd := exec.Command("bash", filepath.Join(scriptDir, "install.sh"))
	cmd.Env = []string{
		"PATH=" + stubBin + ":" + targetDir + ":/usr/bin:/bin",
		"HOME=" + root,
		"GIT_CONFIG_NOSYSTEM=1",
		"FAKE_LIT=" + built,
		"SCHEMA_RELEASE_CHECK=" + schemaReleaseCheck,
		"BUILD_LOG=" + buildLog,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	run.err = cmd.Run()
	run.stdout, run.stderr = stdout.String(), stderr.String()
	if log, err := os.ReadFile(buildLog); err == nil {
		run.built = strings.Fields(string(log))
	}

	// Every run, passing or failing, must leave no staging dir behind.
	entries, err := os.ReadDir(resolvedDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".lit-install.") {
			t.Errorf("staging dir %s left in the target dir", e.Name())
		}
	}
	return run
}

// exitCode is the run's exit status: 0 for a clean exit.
func (r installRun) exitCode(t *testing.T) int {
	t.Helper()
	var exitErr *exec.ExitError
	switch {
	case r.err == nil:
		return 0
	case errors.As(r.err, &exitErr):
		return exitErr.ExitCode()
	default:
		t.Fatalf("install did not run: %v", r.err)
		return -1
	}
}

// TestInstallComparesTheTreeSchemaWithTheNewestRelease pins the guard: a source
// install builds lit only when the newest vX.Y.Z tag's registry reaches the
// tree's schema, refuses before building lit otherwise, and prints the
// comparison it made either way.
func TestInstallComparesTheTreeSchemaWithTheNewestRelease(t *testing.T) {
	t.Parallel()
	tree := treeSchema(t)
	for _, tc := range []struct {
		name       string
		releases   []release
		checkout   string
		exit       int
		comparison string
		refusal    []string
	}{{
		name:       "a tree ahead of the release is refused",
		releases:   []release{{tag: "v0.1.0", schema: tree - 1}},
		exit:       3,
		comparison: fmt.Sprintf("tree=%d release_tag=v0.1.0 release_schema=%d verdict=refuse", tree, tree-1),
		refusal:    []string{fmt.Sprintf("reach schema %d", tree), "v0.1.0, supports schema " + fmt.Sprint(tree-1), "v0.1.0 does not carry these migrations", "scripts/install.sh --latest-release"},
	}, {
		name:       "a tree whose migration differs from the release's at the same version is refused",
		releases:   []release{{tag: "v0.1.0", schema: tree, diverged: true}},
		exit:       3,
		comparison: fmt.Sprintf("tree=%d release_tag=v0.1.0 release_schema=%d verdict=refuse", tree, tree),
		refusal:    []string{fmt.Sprintf("%05d_", tree), "v0.1.0 does not carry these migrations", "scripts/install.sh --latest-release"},
	}, {
		name:       "a tree level with the release installs",
		releases:   []release{{tag: "v0.1.0", schema: tree}},
		comparison: fmt.Sprintf("tree=%d release_tag=v0.1.0 release_schema=%d verdict=install", tree, tree),
	}, {
		name:       "a repo with no release tag is refused",
		exit:       1,
		comparison: fmt.Sprintf("tree=%d release_tag=none release_schema=none verdict=refuse", tree),
		refusal:    []string{"no release tag", "scripts/install.sh --latest-release"},
	}, {
		name:       "a tree with no .git of its own reads no tags, even inside a repo that has them",
		releases:   []release{{tag: "v0.1.0", schema: tree}},
		checkout:   "src",
		exit:       1,
		comparison: fmt.Sprintf("tree=%d release_tag=none release_schema=none verdict=refuse", tree),
		refusal:    []string{"no release tag", "scripts/install.sh --latest-release"},
	}, {
		name:       "a non-release tag is not a release",
		releases:   []release{{tag: "v0.1.0", schema: tree - 1}, {tag: "v0.1.0.1", schema: tree}},
		exit:       3,
		comparison: fmt.Sprintf("tree=%d release_tag=v0.1.0 release_schema=%d verdict=refuse", tree, tree-1),
	}, {
		name:       "the newest release is the highest version, not the last name",
		releases:   []release{{tag: "v0.10.0", schema: tree}, {tag: "v0.9.0", schema: tree - 1}},
		comparison: fmt.Sprintf("tree=%d release_tag=v0.10.0 release_schema=%d verdict=install", tree, tree),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runSourceInstall(t, newLit, tc.releases, tc.checkout)
			if got := run.exitCode(t); got != tc.exit {
				t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", got, tc.exit, run.stdout, run.stderr)
			}
			if want := "schema-release-check: " + tc.comparison + "\n"; !strings.Contains(run.stdout, want) {
				t.Errorf("stdout lacks the comparison %q:\n%s", want, run.stdout)
			}
			for _, want := range tc.refusal {
				if !strings.Contains(run.stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, run.stderr)
				}
			}
			wantLit, wantBuilt := newLit, []string{"./tools/schema-release-check", "./cmd/lit"}
			if tc.exit != 0 {
				wantLit, wantBuilt = oldLit, []string{"./tools/schema-release-check"}
			}
			if got := readFile(t, run.target); got != wantLit {
				t.Errorf("installed lit = %q, want %q", got, wantLit)
			}
			if !slices.Equal(run.built, wantBuilt) {
				t.Errorf("go built %q, want %q", run.built, wantBuilt)
			}
		})
	}
}

// releaseTagSamples is the input every consumer of release-tags.sh's pattern is
// held to it over: each piece of vX.Y.Z broken once (prefix, a missing or extra
// part, a non-digit, an empty part, a leading zero in each part, a suffix),
// beside accepted shapes including the unprefixed form a user types and the
// zero and multi-digit parts a leading-zero rule must still admit.
var releaseTagSamples = []string{
	"v0.18.0", "0.18.0", "v10.0.12", "v0.0.0",
	"v01.2.3", "v1.02.3", "v1.2.03", "v00.1.0",
	"v0.2.0.1", "v0.2", "V0.2.0", "v0.2.0-rc1", "v0.2.0+build", "v0.2.0^{}",
	"vx.y.z", "v0..2", "v0.2.0a", "v0.2.0/../x",
}

// releaseTagsSelects reports whether release-tags.sh's newest_release_tag
// selects tag, in the v-prefixed form every copy is handed after normalizing.
func releaseTagsSelects(t *testing.T, tag string) bool {
	t.Helper()
	selection := exec.Command("bash", "-c", `. ./release-tags.sh && printf '%s\n' "$1" | newest_release_tag`, "_", "v"+strings.TrimPrefix(tag, "v"))
	selected, err := selection.Output()
	if err != nil {
		t.Fatalf("newest_release_tag(%q): %v", tag, err)
	}
	return len(selected) > 0
}

// TestReleaseTagParsesExactlyTheReleaseTags holds release.ParseTag — the gate
// behind lit upgrade --to and lit downgrade --to — to release-tags.sh, and
// holds every tag it accepts to valid semver: lit upgrade decides "already
// current" by semver order, under which an invalid tag sorts below every
// release.
func TestReleaseTagParsesExactlyTheReleaseTags(t *testing.T) {
	t.Parallel()
	for _, tag := range releaseTagSamples {
		parsed, err := litrelease.ParseTag("v" + strings.TrimPrefix(tag, "v"))
		if parses, selects := err == nil, releaseTagsSelects(t, tag); parses != selects {
			t.Errorf("tag %q: release.ParseTag accepts = %v, newest_release_tag accepts = %v", tag, parses, selects)
		}
		if err == nil && !semver.IsValid(parsed.String()) {
			t.Errorf("tag %q: release.ParseTag accepts %q, which semver cannot order", tag, parsed)
		}
	}
}

// TestReleaseKindCutsExactlyTheReleaseTags holds release-kind.sh — which
// decides the version release-validate.yml cuts a tag for — to release-tags.sh:
// a newest CHANGELOG heading is a pending release exactly when v<version> is a
// release tag, and is refused as malformed otherwise, so the pipeline never
// cuts a tag every consumer would refuse.
func TestReleaseKindCutsExactlyTheReleaseTags(t *testing.T) {
	t.Parallel()
	for _, tag := range releaseTagSamples {
		version := strings.TrimPrefix(tag, "v")
		pending := releaseKindCuts(t, "## ["+version+"] - 2026-10-16", version)
		if selects := releaseTagsSelects(t, tag); pending != selects {
			t.Errorf("heading [%s]: release-kind.sh cuts it = %v, newest_release_tag accepts v%s = %v", version, pending, version, selects)
		}
	}
	// The heading around the version is held too: a date is optional, and
	// anything else after the bracket is malformed.
	for heading, want := range map[string]bool{"## [0.5.0]": true, "## [0.5.0]junk": false, "## [0.5.0] 2026-10-16": false} {
		if got := releaseKindCuts(t, heading, "0.5.0"); got != want {
			t.Errorf("heading %q: release-kind.sh cuts it = %v; want %v", heading, got, want)
		}
	}
}

// releaseKindCuts runs release-kind.sh on a CHANGELOG whose newest release
// heading is heading, against an origin with no tags, and reports whether it
// calls version a pending release; it fails the test unless the verdict is
// that or a malformed-heading refusal.
func releaseKindCuts(t *testing.T, heading, version string) bool {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	checkout := filepath.Join(root, "checkout")
	for _, args := range [][]string{{"init", "-q", "--bare", origin}, {"init", "-q", checkout}, {"-C", checkout, "remote", "add", "origin", origin}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(checkout, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"release-kind.sh", "release-tags.sh"} {
		writeFile(t, filepath.Join(checkout, "scripts", name), readFile(t, name), 0o755)
	}
	writeFile(t, filepath.Join(checkout, "CHANGELOG.md"), "# Changelog\n\n## [Unreleased]\n\n"+heading+"\n", 0o644)
	kind := exec.Command("bash", "scripts/release-kind.sh")
	kind.Dir = checkout
	out, err := kind.CombinedOutput()
	pending := err == nil && string(out) == "pending "+version+"\n"
	if malformed := err != nil && strings.Contains(string(out), "release heading is malformed"); !pending && !malformed {
		t.Fatalf("release-kind.sh on %q neither reported %s pending nor refused the heading as malformed (err %v):\n%s", heading, version, err, out)
	}
	return pending
}

// TestReleaseModeAcceptsExactlyTheReleaseTags holds install.sh's release-mode
// copy of the release-tag pattern to release-tags.sh: --from-release accepts a
// tag exactly when newest_release_tag selects it. A stub curl ends every
// accepted run at its first download.
func TestReleaseModeAcceptsExactlyTheReleaseTags(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stubBin := filepath.Join(root, "stub-bin")
	if err := os.MkdirAll(stubBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stubBin, "curl"), "#!/bin/sh\necho 'stub curl' >&2\nexit 22\n", 0o755)
	for _, tag := range releaseTagSamples {
		install := exec.Command("bash", "install.sh", "--from-release", tag)
		install.Env = []string{"PATH=" + stubBin + ":/usr/bin:/bin", "HOME=" + root, "GOBIN=" + filepath.Join(root, "bin")}
		out, _ := install.CombinedOutput()
		var installAccepts bool
		switch {
		case strings.Contains(string(out), "stub curl"):
			installAccepts = true
		case strings.Contains(string(out), "is not a canonical semver release tag"):
			installAccepts = false
		default:
			t.Fatalf("install.sh --from-release %q neither refused the tag nor began a download:\n%s", tag, out)
		}

		if selectionAccepts := releaseTagsSelects(t, tag); installAccepts != selectionAccepts {
			t.Errorf("tag %q: install.sh --from-release accepts = %v, newest_release_tag accepts = %v\ninstall.sh output:\n%s", tag, installAccepts, selectionAccepts, out)
		}
	}
}

// TestInstallRenamesTheStagedBuildOverTheInstalledLit pins the swap: a new
// file takes the installed path, so a process still running the old lit keeps
// its inode, and the version line reports the binary that was installed.
func TestInstallRenamesTheStagedBuildOverTheInstalledLit(t *testing.T) {
	t.Parallel()
	run := runSourceInstall(t, newLit, []release{{tag: "v0.1.0", schema: treeSchema(t)}}, "")
	if run.err != nil {
		t.Fatalf("install failed: %v\nstdout:\n%s\nstderr:\n%s", run.err, run.stdout, run.stderr)
	}
	if got := readFile(t, run.target); got != newLit {
		t.Errorf("installed lit = %q, want the new build", got)
	}
	if inode(t, run.target) == run.oldIno {
		t.Errorf("installed lit kept inode %d: the build was written into the installed file instead of renamed over it", run.oldIno)
	}
	if want := "Installed lit -> " + run.target + "\nlit new\n"; !strings.Contains(run.stdout, want) {
		t.Errorf("stdout lacks the install line followed by the installed version %q:\n%s", want, run.stdout)
	}
}

// TestInstallLeavesTheInstalledLitWhenTheStagedBuildCannotRun pins the gate:
// a build whose `lit version` fails never replaces the working lit, and the
// script says where that lit was left.
func TestInstallLeavesTheInstalledLitWhenTheStagedBuildCannotRun(t *testing.T) {
	t.Parallel()
	run := runSourceInstall(t, brokenLit, []release{{tag: "v0.1.0", schema: treeSchema(t)}}, "")
	var exitErr *exec.ExitError
	if !errors.As(run.err, &exitErr) {
		t.Fatalf("install err = %v, want a non-zero exit\nstdout:\n%s", run.err, run.stdout)
	}
	if got := readFile(t, run.target); got != oldLit {
		t.Errorf("installed lit = %q, want the old one untouched", got)
	}
	if inode(t, run.target) != run.oldIno {
		t.Errorf("installed lit was replaced by a build that cannot run")
	}
	for _, want := range []string{"boom", "nothing was installed at " + run.target} {
		if !strings.Contains(run.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, run.stderr)
		}
	}
	if strings.Contains(run.stdout, "Installed lit") {
		t.Errorf("stdout reports an install that did not happen:\n%s", run.stdout)
	}
}

func writeFile(t *testing.T, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func inode(t *testing.T, name string) uint64 {
	t.Helper()
	fi, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(fi.Sys().(*syscall.Stat_t).Ino)
}
