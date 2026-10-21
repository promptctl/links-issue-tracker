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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// basePrefix starts the name of every base this user's test binaries make in
// os.TempDir(). The uid scopes a sweep to bases this user made and can delete:
// on Linux os.TempDir() is the /tmp every user shares. The pid of the test
// binary that built the base follows the prefix, so a later run can tell a base
// whose owner is still running from one whose owner died.
var basePrefix = fmt.Sprintf("lit-template-%d-", os.Getuid())

// Template is one directory, built on first use and frozen read-only, and the
// copies taken from it. The template lives outside every t.TempDir on purpose,
// so no single test's cleanup can delete it out from under the others; Run
// removes it once every test is over.
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

// Run is the TestMain body of a package whose tests copy templates: it sweeps
// the bases that dead test binaries left behind, runs the tests, removes the
// package's templates, and returns the exit code for os.Exit.
//
// [LAW:no-ambient-temporal-coupling] Removal happens here and only here, after
// m.Run has returned, so no test can ever see a template vanish.
// [LAW:no-silent-failure] A frozen tree left behind is one nothing else will
// delete, so a sweep or a removal that fails fails the run.
func Run(m *testing.M, templates ...*Template) int {
	errs := []error{sweepDead()}
	code := m.Run()
	for _, tp := range templates {
		errs = append(errs, tp.remove())
	}
	if err := errors.Join(errs...); err != nil {
		fmt.Fprintf(os.Stderr, "dirtemplate: %v\n", err)
		code = max(code, 1)
	}
	return code
}

// remove deletes the template, if this run ever built it.
func (tp *Template) remove() error {
	if tp.dir == "" {
		return nil
	}
	return removeBase(filepath.Dir(tp.dir))
}

func (tp *Template) buildFrozen() (_ string, err error) {
	base, err := os.MkdirTemp("", fmt.Sprintf("%s%d-*", basePrefix, os.Getpid()))
	if err != nil {
		return "", err
	}
	// A failed build sweeps its own base: Run removes only templates that
	// finished. [LAW:no-silent-failure] the sweep's own failure joins the
	// build's error.
	defer func() {
		if err != nil {
			err = errors.Join(err, removeBase(base))
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
	if err := freeze(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// freeze makes every file under root read-only and every directory
// unwritable.
func freeze(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return os.Chmod(path, 0o444)
	})
}

// removeBase deletes a template's base, frozen or not. Thawing the
// directories is what makes removal possible at all: unlinking from a 0o555
// directory is exactly what the freeze forbids, while a read-only file in a
// writable directory unlinks fine. Two binaries sweeping one dead base at once
// is the normal case under `go test ./...`, so a path the other sweeper
// already removed is not an error.
func removeBase(base string) error {
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			err = os.Chmod(path, 0o755)
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("thaw %s: %w", base, err)
	}
	return os.RemoveAll(base)
}

// sweepDead removes every base of this user's in os.TempDir() whose owning
// test binary has died. A binary that panicked, hit -timeout or was killed never reached its
// Run's removal, and the frozen tree it left is one `rm -rf` cannot delete. A
// base whose owner is alive — another package's binary running beside this
// one — is left alone.
func sweepDead() error {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return fmt.Errorf("list %s for dead templates: %w", tmp, err)
	}
	var errs []error
	for _, e := range entries {
		if pid, ok := ownerPID(e.Name()); ok && processGone(pid) {
			errs = append(errs, removeBase(filepath.Join(tmp, e.Name())))
		}
	}
	return errors.Join(errs...)
}

// ownerPID reads the owner's pid out of the name of one of this user's bases,
// basePrefix<pid>-<rand>.
func ownerPID(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, basePrefix)
	if !ok {
		return 0, false
	}
	digits, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(digits)
	return pid, err == nil
}

// processGone reports that no process holds pid. os.FindProcess fails for a
// dead pid on Windows; on Unix it always succeeds, and the null signal comes
// back as os.ErrProcessDone. A pid reused by a live process reads as alive, so
// the worst a reuse can do is postpone a sweep, never delete a live template.
func processGone(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	defer p.Release()
	return errors.Is(p.Signal(syscall.Signal(0)), os.ErrProcessDone)
}
