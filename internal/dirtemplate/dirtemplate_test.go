package dirtemplate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/fsperm"
)

const seed = "as built"

// newSeeded is a template holding one file, sub/seed.
func newSeeded() *Template {
	return New("tree", func(dir string) error {
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "sub", "seed"), []byte(seed), 0o644)
	})
}

// seededTemplate is newSeeded, removed when the test ends.
func seededTemplate(t *testing.T) *Template {
	t.Helper()
	tpl := newSeeded()
	t.Cleanup(func() {
		if err := tpl.remove(); err != nil {
			t.Errorf("remove: %v", err)
		}
	})
	return tpl
}

// leave writes residue into a copy: it rewrites the seed and adds a marker
// named for the writer.
func leave(t *testing.T, copyDir, writer string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(copyDir, "sub", "seed"), []byte(writer), 0o644); err != nil {
		t.Fatalf("rewrite the seed in %s's copy: %v", writer, err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, "sub", "marker-"+writer), nil, 0o644); err != nil {
		t.Fatalf("write %s's marker: %v", writer, err)
	}
}

// requireHolds fails unless dir holds exactly the seed reading wantSeed and the
// given markers.
func requireHolds(t *testing.T, dir, wantSeed string, markers ...string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, "sub", "seed"))
	if err != nil {
		t.Fatalf("read the seed in %s: %v", dir, err)
	}
	if string(got) != wantSeed {
		t.Fatalf("seed in %s = %q, want %q", dir, got, wantSeed)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "sub"))
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"seed"}
	for _, m := range markers {
		want = append(want, "marker-"+m)
	}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("%s holds %v, want %v", dir, names, want)
	}
}

// The isolation the fixture exists to give: what one copy's holder writes
// reaches neither the template nor any other copy, whether the copies are
// taken one after another or at the same time. A Template that handed two
// callers one directory fails here.
func TestCopiesShareNothing(t *testing.T) {
	tpl := seededTemplate(t)

	t.Run("in sequence", func(t *testing.T) {
		first := tpl.Copy(t)
		leave(t, first, "first")
		requireHolds(t, tpl.Copy(t), seed)
	})

	t.Run("in parallel", func(t *testing.T) {
		for i := range 8 {
			writer := fmt.Sprintf("w%d", i)
			t.Run(writer, func(t *testing.T) {
				t.Parallel()
				dir := tpl.Copy(t)
				requireHolds(t, dir, seed)
				leave(t, dir, writer)
				requireHolds(t, dir, writer, writer)
			})
		}
	})

	dir, err := tpl.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	requireHolds(t, dir, seed)
}

// A test that reaches the template itself, instead of a copy, is refused at
// its first write, so the mistake is loud rather than residue in every later
// copy.
func TestTheTemplateRefusesWrites(t *testing.T) {
	tpl := seededTemplate(t)
	dir, err := tpl.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	// The modes are asserted before the write probe: the probe skips where the
	// process bypasses permissions, and a template that was never frozen would
	// read to it exactly like that.
	for path, want := range map[string]fs.FileMode{
		filepath.Join(dir, "sub", "seed"): fsperm.FileModeAsStored(t, 0o444),
		filepath.Join(dir, "sub"):         fsperm.DirModeAsStored(t, 0o555),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %v, want %v", path, got, want)
		}
	}
	fsperm.RequireReadOnly(t, filepath.Join(dir, "sub", "seed"))
	if err := os.WriteFile(filepath.Join(dir, "sub", "seed"), []byte("residue"), 0o644); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("write into the template = %v, want a permission error", err)
	}
	requireHolds(t, dir, seed)
}

// A build that fails reports its error to every caller and leaves nothing on
// disk: no later run inherits half a template.
func TestAFailedBuildLeavesNothing(t *testing.T) {
	broken := errors.New("build broke")
	var built string
	tpl := New("tree", func(dir string) error {
		built = dir
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "partial"), nil, 0o644); err != nil {
			return err
		}
		return broken
	})
	for range 2 {
		if _, err := tpl.Dir(); !errors.Is(err, broken) {
			t.Fatalf("Dir = %v, want the build's error", err)
		}
	}
	if _, err := os.Stat(filepath.Dir(built)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the failed build's directory is still there: stat = %v", err)
	}
}

// remove deletes everything the build left, frozen as it is.
func TestRemoveDeletesTheTemplate(t *testing.T) {
	tpl := newSeeded()
	dir, err := tpl.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if err := tpl.remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the template is still there after remove: stat = %v", err)
	}
}

// TestMain runs through Run so a base this package's own failed tests left
// behind is swept by the next run; each test removes its own templates.
func TestMain(m *testing.M) {
	os.Exit(Run(m))
}

// plantBase is a frozen base in os.TempDir(), as a test binary with the given
// pid, run by the user prefix names, leaves one when it dies before its Run
// removes it.
func plantBase(t *testing.T, prefix string, pid int) string {
	t.Helper()
	base, err := os.MkdirTemp("", fmt.Sprintf("%s%d-*", prefix, pid))
	if err != nil {
		t.Fatalf("make a base: %v", err)
	}
	tree := filepath.Join(base, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o755); err != nil {
		t.Fatalf("make the tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tree, "sub", "seed"), []byte(seed), 0o644); err != nil {
		t.Fatalf("write the seed: %v", err)
	}
	if err := freeze(tree); err != nil {
		t.Fatalf("freeze the tree: %v", err)
	}
	return base
}

// runBinary runs this test binary again, testing nothing, so its TestMain's
// Run is all it does, and returns the pid it had.
func runBinary(t *testing.T) int {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^$")
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("run the test binary: %v\n%s", err, out)
	}
	return child.Process.Pid
}

// The next binary's Run deletes the frozen base a dead binary of this user's
// left, and leaves alone the base of a binary that is still running and the
// base of another user's dead binary, which this user could not delete.
func TestRunSweepsOnlyThisUsersBasesOfDeadBinaries(t *testing.T) {
	deadPID := runBinary(t)
	dead := plantBase(t, basePrefix, deadPID)
	live := plantBase(t, basePrefix, os.Getpid())
	othersDead := plantBase(t, fmt.Sprintf("lit-template-%d-", os.Getuid()+1), deadPID)
	t.Cleanup(func() {
		if err := errors.Join(removeBase(live), removeBase(othersDead)); err != nil {
			t.Errorf("remove the bases the sweep must leave: %v", err)
		}
	})

	runBinary(t)

	if _, err := os.Stat(dead); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the dead binary's base survived the sweep: stat = %v", err)
	}
	requireHolds(t, filepath.Join(live, "tree"), seed)
	requireHolds(t, filepath.Join(othersDead, "tree"), seed)
}

// Under `go test ./...` two binaries can sweep one dead base at once; the one
// that finds it already gone has nothing left to do.
func TestRemovingAnAlreadyRemovedBaseSucceeds(t *testing.T) {
	base := plantBase(t, basePrefix, os.Getpid())
	for range 2 {
		if err := removeBase(base); err != nil {
			t.Fatalf("removeBase: %v", err)
		}
	}
}
