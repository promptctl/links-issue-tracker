//go:build unix

// Package scripts holds the behavioral tests of the shell scripts beside it.
package scripts

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const (
	oldLit    = "#!/bin/sh\necho 'lit old'\n"
	newLit    = "#!/bin/sh\necho 'lit new'\n"
	brokenLit = "#!/bin/sh\necho 'boom' >&2\nexit 3\n"
)

// installRun is one source-mode run of install.sh over a target dir that
// already holds a working lit.
type installRun struct {
	target         string // the installed lit, symlinks resolved as install.sh resolves them
	oldIno         uint64
	stdout, stderr string
	err            error
}

// runSourceInstall runs the real install.sh, copied into a scratch checkout, in
// its default source mode. A stub `go` on PATH stands in for the build: it
// writes built to whatever path follows -o, in place, so a build aimed at the
// installed file keeps that file's inode. The two helper scripts install.sh
// sources are replaced by stubs; they set up a real Go build, and the build is
// not what these tests are about.
func runSourceInstall(t *testing.T, built string) installRun {
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
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(scriptDir, "install.sh"), string(script), 0o755)
	writeFile(t, filepath.Join(scriptDir, "cgo-env.sh"), ":\n", 0o644)
	writeFile(t, filepath.Join(scriptDir, "version-ldflags.sh"),
		"export LIT_BUILD_COMMIT=test LIT_BUILD_DATE=test LIT_BUILD_ORIGIN=source\n", 0o644)
	writeFile(t, filepath.Join(stubBin, "go"), `#!/bin/sh
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; fi
  shift
done
printf '%s' "$FAKE_LIT" > "$out"
chmod +x "$out"
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
		"FAKE_LIT=" + built,
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	run.err = cmd.Run()
	run.stdout, run.stderr = stdout.String(), stderr.String()

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

// TestInstallRenamesTheStagedBuildOverTheInstalledLit pins the swap: a new
// file takes the installed path, so a process still running the old lit keeps
// its inode, and the version line reports the binary that was installed.
func TestInstallRenamesTheStagedBuildOverTheInstalledLit(t *testing.T) {
	t.Parallel()
	run := runSourceInstall(t, newLit)
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
	run := runSourceInstall(t, brokenLit)
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
