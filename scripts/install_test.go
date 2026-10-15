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

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
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
	build := exec.Command("go", "build", "-o", schemaReleaseCheck, "../tools/schema-release-check")
	if out, err := build.CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build schema-release-check: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// release is a tag in a fixture repo whose migration registry reaches schema.
type release struct {
	tag    string
	schema int64
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
// release, each commit's internal/store/migrations holding versions 1..schema.
func initFixtureRepo(t *testing.T, root string, releases []release) {
	t.Helper()
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
			writeFile(t, filepath.Join(registry, fmt.Sprintf("%05d_m.sql", v)), "", 0o644)
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

// runSourceInstall runs the real install.sh, copied into a fixture repo carrying
// releases, in its default source mode. A stub `go` on PATH stands in for both
// builds: the schema checker gets the real one built in TestMain, and lit gets
// built, written in place to whatever path follows -o, so a build aimed at the
// installed file keeps that file's inode. The two helper scripts install.sh
// sources for the lit build are replaced by stubs; they set up a real Go build,
// and the build is not what these tests are about.
func runSourceInstall(t *testing.T, built string, releases []release) installRun {
	t.Helper()
	root := t.TempDir()
	scriptDir := filepath.Join(root, "scripts")
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
		exit       int
		comparison string
		refusal    []string
	}{{
		name:       "a tree ahead of the release is refused",
		releases:   []release{{"v0.1.0", tree - 1}},
		exit:       3,
		comparison: fmt.Sprintf("tree=%d release_tag=v0.1.0 release_schema=%d verdict=refuse", tree, tree-1),
		refusal:    []string{fmt.Sprintf("reach schema %d", tree), "v0.1.0, supports schema " + fmt.Sprint(tree-1), "scripts/install.sh --latest-release"},
	}, {
		name:       "a tree level with the release installs",
		releases:   []release{{"v0.1.0", tree}},
		comparison: fmt.Sprintf("tree=%d release_tag=v0.1.0 release_schema=%d verdict=install", tree, tree),
	}, {
		name:       "a repo with no release tag is refused",
		exit:       1,
		comparison: fmt.Sprintf("tree=%d release_tag=none release_schema=none verdict=refuse", tree),
		refusal:    []string{"no release tag", "scripts/install.sh --latest-release"},
	}, {
		name:       "a non-release tag is not a release",
		releases:   []release{{"v0.1.0", tree - 1}, {"v0.1.0.1", tree}},
		exit:       3,
		comparison: fmt.Sprintf("tree=%d release_tag=v0.1.0 release_schema=%d verdict=refuse", tree, tree-1),
	}, {
		name:       "the newest release is the highest version, not the last name",
		releases:   []release{{"v0.10.0", tree}, {"v0.9.0", tree - 1}},
		comparison: fmt.Sprintf("tree=%d release_tag=v0.10.0 release_schema=%d verdict=install", tree, tree),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runSourceInstall(t, newLit, tc.releases)
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
	for _, tag := range []string{"v0.18.0", "0.18.0", "v10.0.12", "v0.2.0.1", "v0.2", "v0.2.0-rc1", "v0.2.0^{}", "vx.y.z", "v0.2.0/../x"} {
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

		selection := exec.Command("bash", "-c", `. ./release-tags.sh && printf '%s\n' "$1" | newest_release_tag`, "_", "v"+strings.TrimPrefix(tag, "v"))
		selected, err := selection.Output()
		if err != nil {
			t.Fatalf("newest_release_tag(%q): %v", tag, err)
		}
		if selectionAccepts := len(selected) > 0; installAccepts != selectionAccepts {
			t.Errorf("tag %q: install.sh --from-release accepts = %v, newest_release_tag accepts = %v\ninstall.sh output:\n%s", tag, installAccepts, selectionAccepts, out)
		}
	}
}

// TestInstallRenamesTheStagedBuildOverTheInstalledLit pins the swap: a new
// file takes the installed path, so a process still running the old lit keeps
// its inode, and the version line reports the binary that was installed.
func TestInstallRenamesTheStagedBuildOverTheInstalledLit(t *testing.T) {
	t.Parallel()
	run := runSourceInstall(t, newLit, []release{{"v0.1.0", treeSchema(t)}})
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
	run := runSourceInstall(t, brokenLit, []release{{"v0.1.0", treeSchema(t)}})
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
