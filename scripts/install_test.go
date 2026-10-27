//go:build unix

// Package scripts holds the behavioral tests of the shell scripts beside it.
package scripts

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	tree, err := migrations.ParseRegistry(names)
	if err != nil {
		t.Fatal(err)
	}
	migrationFiles := map[int64]string{}
	for _, m := range tree.Migrations() {
		migrationFiles[m.Version] = m.Name
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
	writeFile(t, filepath.Join(root, checkout, "go.mod"), "module github.com/promptctl/links-issue-tracker\n\ngo 1.25\n", 0o644)
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

// TestInstallPipedFromStdinSpeaksFirst runs install.sh's release modes as
// curl-piped installs do, read from stdin outside any checkout: the first thing
// on stderr is the script's own, here a stub curl's failure.
func TestInstallPipedFromStdinSpeaksFirst(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--from-release", "v1.2.3"}, {"--latest-release"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			root, env := outsideACheckout(t)
			install := exec.Command("bash", append([]string{"-s", "--"}, args...)...)
			install.Dir, install.Env, install.Stdin = root, env, openFile(t, "install.sh")
			var stderr bytes.Buffer
			install.Stderr = &stderr
			if err := install.Run(); err == nil {
				t.Errorf("exited 0 through a curl that fails every request")
			}
			if !strings.HasPrefix(stderr.String(), "stub curl") {
				t.Errorf("stderr opens with something other than the stub curl's failure:\n%s", stderr.String())
			}
		})
	}
}

// TestInstallSourceBuildRefusesOutsideACheckout runs the source build from
// every place a script with no lit checkout around it can run: stdin, a
// process substitution, and a copy on disk. Each is refused, in the script's
// own words and before the install directory exists.
func TestInstallSourceBuildRefusesOutsideACheckout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		command func(t *testing.T, root string) *exec.Cmd
	}{
		{"read from stdin", func(t *testing.T, _ string) *exec.Cmd {
			install := exec.Command("bash", "-s")
			install.Stdin = openFile(t, "install.sh")
			return install
		}},
		{"a process substitution", func(t *testing.T, _ string) *exec.Cmd {
			return exec.Command("bash", "-c", `bash <(cat "$1")`, "_", mustAbs(t, "install.sh"))
		}},
		{"a copy outside a checkout", func(t *testing.T, root string) *exec.Cmd {
			copied := filepath.Join(root, "dl", "install.sh")
			if err := os.MkdirAll(filepath.Dir(copied), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, copied, readFile(t, "install.sh"), 0o755)
			return exec.Command("bash", copied)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, env := outsideACheckout(t)
			install := tc.command(t, root)
			install.Dir, install.Env = root, env
			var stderr bytes.Buffer
			install.Stderr = &stderr
			var exitErr *exec.ExitError
			if err := install.Run(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Errorf("run = %v, want exit 1", err)
			}
			if want := "error: the source build builds the lit checkout holding this script"; !strings.HasPrefix(stderr.String(), want) {
				t.Errorf("stderr opens with something other than %q:\n%s", want, stderr.String())
			}
			if _, err := os.Stat(filepath.Join(root, "bin")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the refused install touched its install directory: stat = %v", err)
			}
		})
	}
}

// TestInstallPipedHelpPrintsTheHelp runs `bash -s -- --help` as a curl pipe
// would: the help reaches stdout with nothing on stderr.
func TestInstallPipedHelpPrintsTheHelp(t *testing.T) {
	t.Parallel()
	root, env := outsideACheckout(t)
	install := exec.Command("bash", "-s", "--", "--help")
	install.Dir, install.Env, install.Stdin = root, env, openFile(t, "install.sh")
	var stdout, stderr bytes.Buffer
	install.Stdout, install.Stderr = &stdout, &stderr
	if err := install.Run(); err != nil {
		t.Fatalf("--help: %v\nstderr:\n%s", err, stderr.String())
	}
	if want := "install.sh — install `lit` from one of three sources:\n"; !strings.HasPrefix(stdout.String(), want) {
		t.Errorf("stdout opens with something other than %q:\n%s", want, stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr is not empty:\n%s", stderr.String())
	}
}

// outsideACheckout is a directory in no lit checkout and the environment
// install.sh runs under there: PATH holds a curl that fails every request and
// a jq that reads its input, and GOBIN names root/bin, which does not exist.
func outsideACheckout(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	stubBin := filepath.Join(root, "stub-bin")
	if err := os.MkdirAll(stubBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(stubBin, "curl"), "#!/bin/sh\necho 'stub curl' >&2\nexit 22\n", 0o755)
	writeFile(t, filepath.Join(stubBin, "jq"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
	return root, []string{"PATH=" + stubBin + ":/usr/bin:/bin", "HOME=" + root, "GOBIN=" + filepath.Join(root, "bin")}
}

func openFile(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func mustAbs(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(name)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// releaseServer is install.sh's network for one run: a stub curl on PATH that
// answers a request for any URL ending in /<name> with files/<name> when that
// file exists and fails it as a 404 otherwise, recording every requested URL
// and, as "<url> <header>" lines, every header it was sent on stdin (-H @-).
// jq is the host's, linked in, because install.sh's release mode reads the
// manifest with it; nothing else on PATH can hold a lit.
type releaseServer struct {
	root, stubBin, files, requests, headers string
}

func newReleaseServer(t *testing.T) *releaseServer {
	t.Helper()
	root := t.TempDir()
	s := &releaseServer{root: root, stubBin: filepath.Join(root, "stub-bin"), files: filepath.Join(root, "files"), requests: filepath.Join(root, "curl-requests"), headers: filepath.Join(root, "curl-headers")}
	for _, dir := range []string{s.stubBin, s.files} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(s.stubBin, "curl"), `#!/bin/sh
out= url= header=
while [ $# -gt 0 ]; do case $1 in -o) out=$2; shift;; -H) header=$2; shift;; *://*) url=$1;; esac; shift; done
echo "$url" >>'`+s.requests+`'
case $header in
  '') ;;
  @-) while IFS= read -r line; do [ -z "$line" ] || echo "$url $line" >>'`+s.headers+`'; done ;;
  *) echo "$url on argv: $header" >>'`+s.headers+`' ;;
esac
file='`+s.files+`'/${url##*/}
[ -f "$file" ] || exit 22
if [ -n "$out" ]; then cp "$file" "$out"; else cat "$file"; fi
`, 0o755)
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Fatalf("install.sh's release mode needs jq, and this host has none: %v", err)
	}
	if err := os.Symlink(jq, filepath.Join(s.stubBin, "jq")); err != nil {
		t.Fatal(err)
	}
	return s
}

// serve makes name answerable.
func (s *releaseServer) serve(t *testing.T, name string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.files, name), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// serveManifest makes m the release manifest every tag answers with.
func (s *releaseServer) serveManifest(t *testing.T, m litrelease.Manifest) {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	s.serve(t, "release-manifest.json", body)
}

// install runs install.sh with args against the server, installing into
// root/bin, and returns its combined output and the URLs it requested.
func (s *releaseServer) install(t *testing.T, env []string, args ...string) (string, []string, error) {
	t.Helper()
	install := exec.Command("bash", append([]string{"install.sh"}, args...)...)
	install.Env = append([]string{"PATH=" + s.stubBin + ":/usr/bin:/bin", "HOME=" + s.root, "GOBIN=" + filepath.Join(s.root, "bin")}, env...)
	out, err := install.CombinedOutput()
	var requests []string
	if body, readErr := os.ReadFile(s.requests); readErr == nil {
		requests = strings.Fields(string(body))
	} else if !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
	return string(out), requests, err
}

// sentHeaders is every header install.sh sent, as "<url> <header>" lines.
func (s *releaseServer) sentHeaders(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(s.headers)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

// TestInstallLatestReleaseAuthenticatesTheFeedWithTheTokenGhWouldUse holds
// --latest-release's one GitHub API call to gh's token precedence: GH_TOKEN,
// then GITHUB_TOKEN, an empty one counting as unset. The token rides the feed
// request alone, on stdin rather than argv, and the output names which
// variable it came from without ever printing it. The run goes on through the
// manifest to the archive download, so both are seen to go unauthenticated.
func TestInstallLatestReleaseAuthenticatesTheFeedWithTheTokenGhWouldUse(t *testing.T) {
	t.Parallel()
	const (
		gh      = "token-from-GH_TOKEN-5f3a"
		actions = "token-from-GITHUB_TOKEN-9c1e"
		archive = "https://downloads.invalid/lit-linux-amd64.tar.gz"
	)
	feed := litrelease.DefaultLatestReleaseURL
	for _, tc := range []struct {
		name        string
		env         []string
		credentials string
		token       string
	}{
		{"no token", nil, "none", ""},
		{"GITHUB_TOKEN alone", []string{"GITHUB_TOKEN=" + actions}, "GITHUB_TOKEN", actions},
		{"GH_TOKEN alone", []string{"GH_TOKEN=" + gh}, "GH_TOKEN", gh},
		{"GH_TOKEN over GITHUB_TOKEN", []string{"GH_TOKEN=" + gh, "GITHUB_TOKEN=" + actions}, "GH_TOKEN", gh},
		{"empty GH_TOKEN", []string{"GH_TOKEN=", "GITHUB_TOKEN=" + actions}, "GITHUB_TOKEN", actions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := newReleaseServer(t)
			server.serve(t, "latest", []byte(`{"tag_name":"v1.2.3"}`))
			server.serveManifest(t, litrelease.Manifest{Artifacts: []litrelease.Artifact{{Platform: "linux/amd64", URL: archive}}})
			writeFile(t, filepath.Join(server.stubBin, "uname"), "#!/bin/sh\ncase $1 in -s) echo Linux;; -m) echo x86_64;; esac\n", 0o755)
			writeFile(t, filepath.Join(server.stubBin, "tar"), "#!/bin/sh\nexit 1\n", 0o755)
			out, requests, _ := server.install(t, tc.env, "--latest-release")

			manifest := litrelease.DefaultBaseURL + "/v1.2.3/release-manifest.json"
			if want := []string{feed, manifest, archive}; !slices.Equal(requests, want) {
				t.Fatalf("requests %q, want %q\n%s", requests, want, out)
			}
			var want []string
			if tc.token != "" {
				want = []string{feed + " Authorization: Bearer " + tc.token}
			}
			if got := server.sentHeaders(t); !slices.Equal(got, want) {
				t.Errorf("headers sent %q, want %q", got, want)
			}
			if wantOut := "(credentials: " + tc.credentials + ")"; !strings.Contains(out, wantOut) {
				t.Errorf("output lacks %q:\n%s", wantOut, out)
			}
			for _, token := range []string{gh, actions} {
				if strings.Contains(out, token) {
					t.Errorf("output prints a token:\n%s", out)
				}
			}
		})
	}
}

// TestInstallLatestReleaseFeedFailureNamesTheWaysOut: a refused feed request
// (GitHub's 403 at its rate limit, here the stub's failure) stops the install
// with an error naming the credentials it was asked with and both ways on.
func TestInstallLatestReleaseFeedFailureNamesTheWaysOut(t *testing.T) {
	t.Parallel()
	const token = "token-from-GH_TOKEN-5f3a"
	out, requests, err := newReleaseServer(t).install(t, []string{"GH_TOKEN=" + token}, "--latest-release")
	if err == nil {
		t.Fatalf("exited 0 through a feed request that failed:\n%s", out)
	}
	if want := []string{litrelease.DefaultLatestReleaseURL}; !slices.Equal(requests, want) {
		t.Errorf("requests %q, want %q", requests, want)
	}
	for _, want := range []string{
		"error: could not fetch " + litrelease.DefaultLatestReleaseURL + " (credentials: GH_TOKEN)",
		"(set GH_TOKEN or GITHUB_TOKEN)",
		"--from-release <tag>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, token) {
		t.Errorf("output prints the token:\n%s", out)
	}
}

// TestInstallFetchesTheReleaseURLsLitUpgradeFetches holds install.sh's release
// URLs to release.HTTPResolver's defaults by what each one requests first: the
// same latest-release feed, and the same release manifest. Stub transports
// record each request and fail it, so nothing reaches the network.
func TestInstallFetchesTheReleaseURLsLitUpgradeFetches(t *testing.T) {
	t.Parallel()
	const tag = "v1.2.3"
	installRequest := func(args ...string) string {
		t.Helper()
		out, requests, _ := newReleaseServer(t).install(t, nil, args...)
		if len(requests) != 1 {
			t.Fatalf("install.sh %v: want exactly one curl request before the stub's failure stops it, got %q\n%s", args, requests, out)
		}
		return requests[0]
	}

	var litRequests []string
	resolver := &litrelease.HTTPResolver{Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		litRequests = append(litRequests, req.URL.String())
		return nil, errors.New("stub transport")
	})}}
	if _, err := resolver.LatestTag(context.Background()); err == nil {
		t.Fatal("LatestTag succeeded through a transport that fails every request")
	}
	parsed, err := litrelease.ParseTag(tag)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), parsed, "linux/amd64"); err == nil {
		t.Fatal("Resolve succeeded through a transport that fails every request")
	}
	if len(litRequests) != 2 {
		t.Fatalf("want one request each from LatestTag and Resolve, got %q", litRequests)
	}

	if install, lit := installRequest("--latest-release"), litRequests[0]; install != lit {
		t.Errorf("latest-release feed: install.sh requests %q, lit upgrade requests %q", install, lit)
	}
	if install, lit := installRequest("--from-release", tag), litRequests[1]; install != lit {
		t.Errorf("release %s manifest: install.sh requests %q, lit upgrade requests %q", tag, install, lit)
	}
}

// TestInstallDownloadsTheArtifactTheReleaseManifestListsForItsPlatform holds
// install.sh to the release manifest lit upgrade installs from: on every
// platform, install.sh downloads the URL the manifest lists for that platform,
// and refuses, having requested nothing but the manifest, when the manifest
// lists none or lists an archive that is neither a .tar.gz nor a .zip. The
// manifest is unlike any release goreleaser builds, so a table of platforms or
// archive names inside install.sh cannot pass. Stub uname reports the platform.
func TestInstallDownloadsTheArtifactTheReleaseManifestListsForItsPlatform(t *testing.T) {
	t.Parallel()
	const tag = "v1.2.3"
	manifestURL := litrelease.DefaultBaseURL + "/" + tag + "/release-manifest.json"
	const base = "https://downloads.invalid/any/path/"
	manifest := litrelease.Manifest{Artifacts: []litrelease.Artifact{
		{Platform: "linux/amd64", URL: base + "for-linux-amd64.tar.gz"},
		{Platform: "darwin/arm64", URL: base + "for-darwin-arm64.tar.gz"},
		{Platform: "windows/amd64", URL: base + "for-windows-amd64.zip"},
		{Platform: "freebsd/amd64", URL: base + "for-freebsd-amd64.tar.gz"},
		{Platform: "linux/386", URL: base + "for-linux-386.7z"},
	}}
	// What uname reports on each platform, as Go names that platform: the
	// test's own knowledge, independent of install.sh's mapping.
	kernels := map[string]string{
		"Linux":                 "linux",
		"Darwin":                "darwin",
		"MINGW64_NT-10.0-22631": "windows",
		"MSYS_NT-10.0-22631":    "windows",
		"CYGWIN_NT-10.0-22631":  "windows",
		"FreeBSD":               "freebsd",
	}
	machines := map[string]string{
		"x86_64":  "amd64",
		"amd64":   "amd64",
		"aarch64": "arm64",
		"arm64":   "arm64",
		"i686":    "386",
		"armv7l":  "arm",
	}
	for kernel, goos := range kernels {
		for machine, goarch := range machines {
			platform := goos + "/" + goarch
			t.Run(kernel+" "+machine, func(t *testing.T) {
				t.Parallel()
				server := newReleaseServer(t)
				server.serveManifest(t, manifest)
				// The extractor probes run before the archive download; stubs
				// make them pass on hosts that lack one.
				for _, tool := range []string{"tar", "unzip"} {
					writeFile(t, filepath.Join(server.stubBin, tool), "#!/bin/sh\nexit 1\n", 0o755)
				}
				writeFile(t, filepath.Join(server.stubBin, "uname"), "#!/bin/sh\ncase $1 in -s) echo '"+kernel+"';; -m) echo '"+machine+"';; esac\n", 0o755)
				out, requests, _ := server.install(t, nil, "--from-release", tag)

				wantRequests, wantOut := []string{manifestURL}, "has no artifact for platform "+platform+" (available"
				if i := slices.IndexFunc(manifest.Artifacts, func(a litrelease.Artifact) bool { return a.Platform == platform }); i >= 0 {
					switch url := manifest.Artifacts[i].URL; {
					case strings.HasSuffix(url, ".tar.gz"), strings.HasSuffix(url, ".zip"):
						wantRequests, wantOut = append(wantRequests, url), "Downloading "+url
					default:
						wantOut = "lists " + url + " for " + platform + ", which is neither a .tar.gz nor a .zip archive"
					}
				}
				if !slices.Equal(requests, wantRequests) {
					t.Errorf("%s: install.sh requests %q, want %q\n%s", platform, requests, wantRequests, out)
				}
				if !strings.Contains(out, wantOut) {
					t.Errorf("%s: output lacks %q:\n%s", platform, wantOut, out)
				}
			})
		}
	}
}

// TestInstallFromReleaseInstallsOnlyTheArchiveTheManifestDigestNames runs
// release mode end to end on this host's real platform, in each archive
// format: the archive installs when its SHA256 is the one the manifest lists,
// and nothing is installed when it is not. The zip case's tar reads nothing,
// as GNU tar reads no zip.
func TestInstallFromReleaseInstallsOnlyTheArchiveTheManifestDigestNames(t *testing.T) {
	t.Parallel()
	archives := map[string][]byte{
		"lit-archive.tar.gz": tarGz(t, map[string]string{"lit": newLit}),
		"lit-archive.zip":    zipArchive(t, map[string]string{"lit": newLit}),
	}
	listed := func(name string) string {
		digest := sha256.Sum256(archives[name])
		return hex.EncodeToString(digest[:])
	}
	for name, tc := range map[string]struct {
		archive, sha256 string
		install         bool
	}{
		"tar.gz, the listed digest": {archive: "lit-archive.tar.gz", sha256: listed("lit-archive.tar.gz"), install: true},
		"zip, the listed digest":    {archive: "lit-archive.zip", sha256: listed("lit-archive.zip"), install: true},
		"tar.gz, another digest":    {archive: "lit-archive.tar.gz", sha256: strings.Repeat("0", 64), install: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			url := "https://downloads.invalid/v1.2.3/" + tc.archive
			server := newReleaseServer(t)
			server.serveManifest(t, litrelease.Manifest{Artifacts: []litrelease.Artifact{{Platform: litrelease.CurrentPlatform(), URL: url, SHA256: tc.sha256}}})
			server.serve(t, tc.archive, archives[tc.archive])
			if strings.HasSuffix(tc.archive, ".zip") {
				writeFile(t, filepath.Join(server.stubBin, "tar"), "#!/bin/sh\necho 'tar: not a tar archive' >&2\nexit 2\n", 0o755)
			}
			out, _, err := server.install(t, nil, "--from-release", "v1.2.3")
			installed := filepath.Join(server.root, "bin", "lit")
			if tc.install {
				if err != nil {
					t.Fatalf("install.sh: %v\n%s", err, out)
				}
				if got := readFile(t, installed); got != newLit {
					t.Errorf("%s holds %q, want the archive's lit %q", installed, got, newLit)
				}
				return
			}
			if err == nil {
				t.Fatalf("install.sh succeeded on an archive whose digest the manifest does not list:\n%s", out)
			}
			if want := "error: SHA256 mismatch for " + url; !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
			if _, err := os.Stat(installed); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s exists after a digest mismatch (stat: %v)", installed, err)
			}
		})
	}
}

// zipArchive is a zip of regular files, mode 0755, as goreleaser builds a
// Windows release archive.
func zipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, body := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o755)
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// tarGz is a gzipped tar of regular files, mode 0755, as goreleaser builds a
// release archive.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

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
