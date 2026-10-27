// Package atomicfile replaces a file's contents atomically: the new contents
// are written to a temporary sibling and renamed over the target, so a reader
// sees the old file or the new one, never a torn write.
//
// [LAW:one-source-of-truth] Every file lit replaces this way goes through
// Replace, so the temp naming, the mode, the fsync and the cleanup on failure
// are decided once. It imports only the standard library, which is what lets
// the stdlib-only statusline writer share it (.golangci.yml).
package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Durability says whether a replacement must survive a crash of the machine,
// not only of the process. A rename is atomic against other processes either
// way; without an fsync, a power loss can still leave the new name pointing at
// contents that never reached the disk. The zero Durability is neither, so one
// nobody set panics instead of quietly skipping the fsync.
type Durability int

const (
	// Replaceable is for a file the next writer rewrites anyway, or one whose
	// loss only costs recomputing it: a crash that loses the newest version
	// costs nothing an fsync would be worth.
	Replaceable Durability = iota + 1
	// Durable is for a file nothing recomputes: its contents are fsynced
	// before the rename, so a crash leaves the old file or the whole new one,
	// and its directory is fsynced after, where the platform can, so the
	// rename itself survives too.
	Durable
)

// tempInfix names a write's temporary file <name>.tmp-<digits>, the digits
// being what os.CreateTemp draws. It extends the target's name, so a pattern
// that covers name* covers an in-flight write too (the statusline's git
// exclude), and it never ends in the target's extension, so a reader that
// picks files by extension (backup listing keeps *.json) passes it over.
const tempInfix = ".tmp-"

// TempTarget reports the name of the target a file named name is the
// temporary file of, and whether name is one at all. It is the one parser of
// the names Replace gives its temporary files, so a directory's owner that
// knows no write into it is running can tell what a killed write left there
// from everything else in it.
func TempTarget(name string) (target string, ok bool) {
	i := strings.LastIndex(name, tempInfix)
	if i <= 0 {
		return "", false
	}
	digits := name[i+len(tempInfix):]
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return "", false
	}
	return name[:i], true
}

// WriteFile replaces name with data, as Replace does.
func WriteFile(name string, data []byte, perm fs.FileMode, durability Durability) error {
	return Replace(name, perm, durability, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// Replace writes name's new contents through write into a temporary sibling,
// sets its mode to perm, and renames it over name. name's directory must
// exist. On any failure name is untouched and the temporary file is removed;
// an error from write comes back wrapped but not reworded, so errors.Is and
// errors.As still reach it, and Replace's own steps are named in theirs.
//
// The temporary file is created before write runs, so a directory that cannot
// be written fails before write does any work (a download, say).
func Replace(name string, perm fs.FileMode, durability Durability, write func(io.Writer) error) error {
	syncFile, syncParent := durability.syncs()
	dir := filepath.Dir(name)
	// A sibling, on the target's filesystem, so the rename is atomic.
	tmp, err := os.CreateTemp(dir, filepath.Base(name)+tempInfix+"*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	if err := fill(tmp, perm, syncFile, write); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), name); err != nil {
		return errors.Join(fmt.Errorf("rename: %w", err), os.Remove(tmp.Name()))
	}
	syncParent(dir)
	return nil
}

// fill writes the temporary file to its final contents and mode and closes
// it, on every path.
func fill(tmp *os.File, perm fs.FileMode, syncFile func(*os.File) error, write func(io.Writer) error) error {
	if err := write(tmp); err != nil {
		return errors.Join(err, tmp.Close())
	}
	// By handle, so the mode is perm exactly, whatever the umask.
	if err := tmp.Chmod(perm); err != nil {
		return errors.Join(fmt.Errorf("chmod: %w", err), tmp.Close())
	}
	if err := syncFile(tmp); err != nil {
		return errors.Join(fmt.Errorf("fsync: %w", err), tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// syncs returns the fsync steps a write at d runs on the file and then on its
// directory.
// [LAW:dataflow-not-control-flow] The one branch in a write is the domain's
// own enum, read here once, before anything touches the disk, so an illegal
// Durability leaves nothing behind; Replace runs the same steps for both.
func (d Durability) syncs() (file func(*os.File) error, dir func(string)) {
	switch d {
	case Replaceable:
		return func(*os.File) error { return nil }, func(string) {}
	case Durable:
		return (*os.File).Sync, syncDir
	}
	panic(fmt.Sprintf("atomicfile: illegal Durability %d", d))
}

// syncDir fsyncs dir, where the platform can, so the rename into it survives a
// crash too.
func syncDir(dir string) {
	// [LAW:no-silent-failure] exception: the directory fsync that makes the
	// renamed entry itself crash-durable is not supported on every platform
	// (Windows cannot fsync a directory handle), and there is no recovery
	// action when it fails. The file's own fsync, which fails loudly, is the
	// load-bearing one: with the contents on disk, a miss here can only roll a
	// crash back to the old file, never tear the new.
	if f, err := os.Open(dir); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
}
