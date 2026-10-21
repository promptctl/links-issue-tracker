// Package dirtemplate builds a directory once per test binary and hands each
// test its own copy of it: a test fixture whose setup is expensive (a store
// brought up through its whole migration chain) and whose copy is cheap.
//
// What goes into the directory is the caller's: a Template runs the build
// function it was given and knows nothing else about the contents, so the
// fixture a package hands out is whatever its own production code builds —
// never a second description of it. [LAW:one-source-of-truth]
package dirtemplate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Template is one directory, built on first use and frozen read-only, and the
// copies taken from it. The template lives outside every t.TempDir on purpose,
// so no single test's cleanup can delete it out from under the others; the
// package's TestMain removes it once the run is over.
//
// [LAW:no-shared-mutable-globals] The built directory is shared by every test
// in the binary, so it has one owner: it is written only by the build, once,
// under the sync.Once, and is read-only to everything after.
type Template struct {
	name  string
	build func(dir string) error

	once sync.Once
	dir  string
	err  error
}

// New is a template whose build writes the directory at the path it is handed,
// a path that does not exist yet. Every copy is named name, so a copy sits
// where the code under test expects the directory the build made.
func New(name string, build func(dir string) error) *Template {
	return &Template{name: name, build: build}
}

// Dir is the frozen template, built on the first call. A build that failed
// fails every call, with the build's error, and leaves nothing on disk.
func (tp *Template) Dir() (string, error) {
	tp.once.Do(func() { tp.dir, tp.err = tp.buildFrozen() })
	return tp.dir, tp.err
}

// Copy is a private, writable copy of the template inside a fresh t.TempDir(),
// so the copy vanishes with the test and no two tests ever share a path.
func (tp *Template) Copy(t testing.TB) string {
	t.Helper()
	dir, err := tp.Dir()
	if err != nil {
		t.Fatalf("build the %s template: %v", tp.name, err)
	}
	dst := filepath.Join(t.TempDir(), tp.name)
	// os.CopyFS re-derives file modes (0o666 before umask), so the copy is
	// writable even though the template's own files are frozen read-only.
	if err := os.CopyFS(dst, os.DirFS(dir)); err != nil {
		t.Fatalf("copy the %s template: %v", tp.name, err)
	}
	return dst
}

// Remove deletes the template, if it was ever built. The thaw before RemoveAll
// is what makes removal possible at all: unlinking from a 0o555 directory is
// exactly what the freeze forbids.
func (tp *Template) Remove() error {
	if tp.dir == "" {
		return nil
	}
	if err := freeze(tp.dir, 0o644, 0o755); err != nil {
		return fmt.Errorf("thaw the %s template: %w", tp.name, err)
	}
	return os.RemoveAll(filepath.Dir(tp.dir))
}

func (tp *Template) buildFrozen() (_ string, err error) {
	base, err := os.MkdirTemp("", "lit-template-")
	if err != nil {
		return "", err
	}
	// A failed build sweeps its own base: TestMain removes only templates that
	// finished, so nothing else would. Thaw first, since a build that failed
	// mid-freeze left directories RemoveAll cannot unlink from.
	// [LAW:no-silent-failure] the sweep's own failure joins the build's error.
	defer func() {
		if err != nil {
			err = errors.Join(err, freeze(base, 0o644, 0o755), os.RemoveAll(base))
		}
	}()
	dir := filepath.Join(base, tp.name)
	if err := tp.build(dir); err != nil {
		return "", err
	}
	// Frozen so a test that reaches the template itself — instead of a copy —
	// fails loudly at its first write rather than leaking state into every
	// later copy. Directories are frozen too, not just files: rename(2), the
	// write-new-then-rename-over pattern behind atomic swaps, is gated by the
	// containing directory's write bit. Permission bits are inert for a process
	// that bypasses them (root); a package's residue test is the proof of
	// isolation, and the freeze only makes the common mistake loud.
	// [LAW:no-silent-failure]
	if err := freeze(dir, 0o444, 0o555); err != nil {
		return "", err
	}
	return dir, nil
}

// freeze stamps one mode onto every file and another onto every directory
// under root — the same walk freezes a finished template and thaws it, so the
// two modes cannot drift apart. [LAW:one-source-of-truth]
func freeze(root string, fileMode, dirMode fs.FileMode) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, dirMode)
		}
		return os.Chmod(path, fileMode)
	})
}
