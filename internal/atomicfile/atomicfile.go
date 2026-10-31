// Package atomicfile writes a file's contents atomically: the new contents
// are written to a temporary sibling and then given the target's name, so a
// reader sees the old file or the new one, never a torn write. Replace renames
// over the target; Publish links to it, so it creates the target only where
// none exists.
//
// [LAW:one-source-of-truth] Every file lit writes this way goes through
// Replace or Publish, so the temp naming, the mode, the fsync and the cleanup
// on failure are decided once. It imports only the standard library, which is what lets
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

// Durability says whether a write must survive a crash of the machine, not
// only of the process. A rename or a link is atomic against other processes
// either way; without an fsync, a power loss can still leave the name pointing
// at contents that never reached the disk. The zero Durability is neither, so one
// nobody set panics instead of quietly skipping the fsync.
type Durability int

const (
	// Replaceable is for a file the next writer rewrites anyway, or one whose
	// loss only costs recomputing it: a crash that loses the newest version
	// costs nothing an fsync would be worth.
	Replaceable Durability = iota + 1
	// Durable is for a file nothing recomputes: its contents are fsynced
	// before it takes the name, so a crash leaves the old state or the whole
	// new file, and its directory is fsynced after, where the platform can,
	// so the name itself survives too.
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
// the names a write gives its temporary files, so a directory's owner that
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
	return Replace(name, perm, durability, writeBytes(data))
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
	tmp, err := stage(name, perm, syncFile, write)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		return errors.Join(fmt.Errorf("rename: %w", err), os.Remove(tmp))
	}
	syncParent(filepath.Dir(name))
	return nil
}

// Publish creates name holding the contents write produces, at mode perm,
// only if name does not exist: write-once, the way Replace is
// last-writer-wins. nil means name exists: this call's file, or one that was
// already there, whatever it holds, left as it was. name's directory must
// exist.
//
// [LAW:single-enforcer] os.Link is what refuses an existing name: a rename
// would replace it. Linking a completed temporary file, rather than creating
// name with O_EXCL and filling it, means name is never visible half-written,
// and a process killed mid-write leaves no name at all. Under Durable the
// contents are on disk before the link, so a crash cannot leave the name
// pointing at bytes that never landed.
//
// A link the filesystem cannot make comes back as the *os.LinkError os.Link
// returned, so a caller can name that requirement for the step that has it
// and no other.
func Publish(name string, perm fs.FileMode, durability Durability, write func(io.Writer) error) error {
	syncFile, syncParent := durability.syncs()
	tmp, err := stage(name, perm, syncFile, write)
	if err != nil {
		return err
	}
	linkErr := os.Link(tmp, name)
	if errors.Is(linkErr, fs.ErrExist) {
		linkErr = nil
	}
	if linkErr != nil {
		return errors.Join(linkErr, os.Remove(tmp))
	}
	// [LAW:no-silent-failure] exception: name is settled, so a temporary file
	// that will not go is no failure of the publish; there is no recovery
	// action, and what it leaves is litter TempTarget can name.
	_ = os.Remove(tmp)
	syncParent(filepath.Dir(name))
	return nil
}

// PublishFile creates name holding data, as Publish does.
func PublishFile(name string, data []byte, perm fs.FileMode, durability Durability) error {
	return Publish(name, perm, durability, writeBytes(data))
}

// writeBytes is the write that produces data.
func writeBytes(data []byte) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}
}

// stage writes name's contents through write into a temporary sibling and
// returns its path; on failure the sibling is gone. A sibling, on the target's
// filesystem, so a rename or link to name is atomic.
func stage(name string, perm fs.FileMode, syncFile func(*os.File) error, write func(io.Writer) error) (string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+tempInfix+"*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	if err := fill(tmp, perm, syncFile, write); err != nil {
		return "", errors.Join(err, os.Remove(tmp.Name()))
	}
	return tmp.Name(), nil
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

// syncDir fsyncs dir, where the platform can, so a name made in it survives a
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
