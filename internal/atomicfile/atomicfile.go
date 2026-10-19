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
// contents that never reached the disk.
type Durability int

const (
	// Replaceable is for a file the next writer rewrites anyway, or one whose
	// loss only costs recomputing it: a crash that loses the newest version
	// costs nothing an fsync would be worth.
	Replaceable Durability = iota
	// Durable is for a file nothing recomputes: its contents are fsynced
	// before the rename, so a crash leaves the old file or the whole new one,
	// and its directory is fsynced after, where the platform can, so the
	// rename itself survives too.
	Durable
)

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
// an error from write is returned as write gave it, and Replace's own steps
// are named in theirs.
//
// The temporary file is created before write runs, so a directory that cannot
// be written fails before write does any work (a download, say).
func Replace(name string, perm fs.FileMode, durability Durability, write func(io.Writer) error) error {
	dir := filepath.Dir(name)
	// A hidden sibling: on the target's filesystem, so the rename is atomic,
	// and out of a plain listing, so a reader that lists the directory (backup
	// listing keeps *.json) does not mistake an in-flight write for a file.
	tmp, err := os.CreateTemp(dir, "."+strings.TrimPrefix(filepath.Base(name), ".")+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	if err := fill(tmp, perm, durability, write); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), name); err != nil {
		return errors.Join(fmt.Errorf("rename: %w", err), os.Remove(tmp.Name()))
	}
	durability.syncDir(dir)
	return nil
}

// fill writes the temporary file to its final contents and mode and closes
// it, on every path.
func fill(tmp *os.File, perm fs.FileMode, durability Durability, write func(io.Writer) error) error {
	if err := write(tmp); err != nil {
		return errors.Join(err, tmp.Close())
	}
	// By handle, so the mode is perm exactly, whatever the umask.
	if err := tmp.Chmod(perm); err != nil {
		return errors.Join(fmt.Errorf("chmod: %w", err), tmp.Close())
	}
	if err := durability.syncFile(tmp); err != nil {
		return errors.Join(fmt.Errorf("fsync: %w", err), tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// [LAW:dataflow-not-control-flow] The one branch in a write is the domain's
// own enum, read in these two methods; Replace runs the same steps for both.
func (d Durability) syncFile(f *os.File) error {
	switch d {
	case Replaceable:
		return nil
	case Durable:
		return f.Sync()
	}
	panic(fmt.Sprintf("atomicfile: illegal Durability %d", d))
}

func (d Durability) syncDir(dir string) {
	switch d {
	case Replaceable:
		return
	case Durable:
		// [LAW:no-silent-failure] exception: the directory fsync that makes
		// the renamed entry itself crash-durable is not supported on every
		// platform (Windows cannot fsync a directory handle), and there is no
		// recovery action when it fails. The file's own fsync, which fails
		// loudly, is the load-bearing one: with the contents on disk, a miss
		// here can only roll a crash back to the old file, never tear the new.
		if f, err := os.Open(dir); err == nil {
			_ = f.Sync()
			_ = f.Close()
		}
		return
	}
	panic(fmt.Sprintf("atomicfile: illegal Durability %d", d))
}
