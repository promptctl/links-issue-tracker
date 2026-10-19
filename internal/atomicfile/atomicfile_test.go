package atomicfile

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var durabilities = []Durability{Replaceable, Durable}

func TestWriteFileCreatesTheFileWithExactlyItsMode(t *testing.T) {
	for _, durability := range durabilities {
		dir := t.TempDir()
		name := filepath.Join(dir, "config.json")
		// 0o666 is what a umask of 022 would turn into 0o644, so only a mode
		// set regardless of the umask reads back as 0o666.
		if err := WriteFile(name, []byte("new"), 0o666, durability); err != nil {
			t.Fatalf("WriteFile(%d) = %v", durability, err)
		}
		assertFile(t, name, "new", 0o666)
		assertOnlyEntry(t, dir, "config.json")
	}
}

func TestWriteFileReplacesAnExistingFileAndItsMode(t *testing.T) {
	for _, durability := range durabilities {
		dir := t.TempDir()
		name := filepath.Join(dir, "lit")
		if err := os.WriteFile(name, []byte("old binary"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := WriteFile(name, []byte("new binary"), 0o755, durability); err != nil {
			t.Fatalf("WriteFile(%d) = %v", durability, err)
		}
		assertFile(t, name, "new binary", 0o755)
		assertOnlyEntry(t, dir, "lit")
	}
}

func TestAFailedWriteLeavesTheTargetAndNoTempFile(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "lit")
	if err := os.WriteFile(name, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("checksum mismatch")
	err := Replace(name, 0o755, Durable, func(w io.Writer) error {
		if _, err := w.Write([]byte("half a bin")); err != nil {
			return err
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("Replace() = %v, want the write's own error", err)
	}
	assertFile(t, name, "old binary", 0o755)
	assertOnlyEntry(t, dir, "lit")
}

// A rename can fail after the contents are written in full; the temp file
// holding them is removed then too.
func TestAFailedRenameLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "receive-block.pending")
	// A non-empty directory cannot be renamed over.
	if err := os.MkdirAll(filepath.Join(name, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(name, []byte("block\n"), 0o600, Replaceable); err == nil {
		t.Fatal("WriteFile over a non-empty directory succeeded; want the rename's error")
	}
	assertOnlyEntry(t, dir, "receive-block.pending")
}

func TestAMissingDirectoryIsAnErrorAndCreatesNothing(t *testing.T) {
	parent := t.TempDir()
	name := filepath.Join(parent, "absent", "config.json")
	if err := WriteFile(name, []byte("{}"), 0o644, Durable); err == nil {
		t.Fatal("WriteFile into a missing directory succeeded; want an error")
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("parent holds %v after the failed write, want nothing", entries)
	}
}

// A reader that picks a directory's files by extension — backup listing
// keeps *.json — must not see a write that is still in flight.
func TestAnInFlightWriteIsNotListedAsTheTargetsKind(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "20261019-000000.000000000.json")
	err := Replace(name, 0o644, Replaceable, func(w io.Writer) error {
		listed, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			return err
		}
		if len(listed) != 0 {
			t.Errorf("*.json lists %v while the write is in flight, want nothing", listed)
		}
		_, err = w.Write([]byte("{}"))
		return err
	})
	if err != nil {
		t.Fatalf("Replace() = %v", err)
	}
}

func TestAnIllegalDurabilityPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("WriteFile with an illegal Durability returned; want a panic")
		}
	}()
	_ = WriteFile(filepath.Join(t.TempDir(), "x"), nil, 0o644, Durability(7))
}

func assertFile(t *testing.T, name, contents string, perm fs.FileMode) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil || string(got) != contents {
		t.Fatalf("%s = %q, %v; want %q", name, got, err, contents)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != perm {
		t.Fatalf("%s mode = %v, want %v", name, info.Mode().Perm(), perm)
	}
}

func assertOnlyEntry(t *testing.T, dir, want string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{want}) {
		t.Fatalf("%s holds %v, want only %s", dir, names, want)
	}
}
