package dirtemplate_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/dirtemplate"
	"github.com/promptctl/links-issue-tracker/internal/fsperm"
)

const seed = "as built"

// newSeeded is a template holding one file, sub/seed.
func newSeeded() *dirtemplate.Template {
	return dirtemplate.New("tree", func(dir string) error {
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "sub", "seed"), []byte(seed), 0o644)
	})
}

// seededTemplate is newSeeded, removed when the test ends.
func seededTemplate(t *testing.T) *dirtemplate.Template {
	t.Helper()
	tpl := newSeeded()
	t.Cleanup(func() {
		if err := tpl.Remove(); err != nil {
			t.Errorf("Remove: %v", err)
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
	tpl := dirtemplate.New("tree", func(dir string) error {
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

// Remove deletes everything the build left, frozen as it is.
func TestRemoveDeletesTheTemplate(t *testing.T) {
	tpl := newSeeded()
	dir, err := tpl.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if err := tpl.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the template is still there after Remove: stat = %v", err)
	}
}
