package atomicfile

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/fsperm"
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
	if !errors.Is(err, refused) || err.Error() != refused.Error() {
		t.Fatalf("Replace() = %v, want the write's own error, not reworded", err)
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

// A write still in flight is covered by a pattern for the target's name — the
// statusline's git exclude is /.lit/statusline.json* — and not by one for the
// target's extension — backup listing keeps *.json.
func TestAnInFlightWriteMatchesTheTargetsNameButNotItsExtension(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "statusline.json")
	err := Replace(name, 0o644, Replaceable, func(w io.Writer) error {
		byName, err := filepath.Glob(name + "*")
		if err != nil {
			return err
		}
		if len(byName) != 1 {
			t.Errorf("statusline.json* lists %v while the write is in flight, want the temp file alone", byName)
		}
		byExtension, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			return err
		}
		if len(byExtension) != 0 {
			t.Errorf("*.json lists %v while the write is in flight, want nothing", byExtension)
		}
		_, err = w.Write([]byte("{}"))
		return err
	})
	if err != nil {
		t.Fatalf("Replace() = %v", err)
	}
}

// The temporary file of a write in flight parses back to its target, and no
// other name parses at all, so an owner removing what killed writes left in
// its directory removes nothing else.
func TestTempTargetParsesAnInFlightWriteAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "snapshot.json")
	err := Replace(name, 0o644, Replaceable, func(w io.Writer) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(entries) != 1 {
			t.Fatalf("directory holds %v while the write is in flight, want the temp file alone", entries)
		}
		if target, ok := TempTarget(entries[0].Name()); !ok || target != "snapshot.json" {
			t.Errorf("TempTarget(%q) = %q, %v; want snapshot.json, true", entries[0].Name(), target, ok)
		}
		_, err = w.Write([]byte("{}"))
		return err
	})
	if err != nil {
		t.Fatalf("Replace() = %v", err)
	}
	for _, other := range []string{"snapshot.json", "snapshot.json.tmp", "snapshot.json.tmp-", "snapshot.json.tmp-12a", "snapshot.tmp-1.json", ".tmp-123"} {
		if target, ok := TempTarget(other); ok {
			t.Errorf("TempTarget(%q) = %q, true; want no target", other, target)
		}
	}
}

// The zero Durability is illegal too, so one nobody set cannot pass for
// Replaceable; and the panic comes before anything is written.
func TestAnIllegalDurabilityPanicsAndLeavesNothing(t *testing.T) {
	for _, durability := range []Durability{0, 7} {
		dir := t.TempDir()
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("WriteFile with Durability %d returned; want a panic", durability)
				}
			}()
			_ = WriteFile(filepath.Join(dir, "x"), nil, 0o644, durability)
		}()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("Durability %d left %v behind, want nothing", durability, entries)
		}
	}
}

// assertFile checks name holds contents with the mode perm reads back as on
// this platform.
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
	if want := fsperm.FileModeAsStored(t, perm); info.Mode().Perm() != want {
		t.Fatalf("%s mode = %v, want %v (%v as this platform stores it)", name, info.Mode().Perm(), want, perm)
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

func writeString(s string) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	}
}

func TestPublishCreatesTheFileWithItsContentsAndMode(t *testing.T) {
	for _, durability := range durabilities {
		dir := t.TempDir()
		name := filepath.Join(dir, "token")
		published, err := Publish(name, 0o666, durability, writeString("first"))
		if err != nil || !published {
			t.Fatalf("Publish(%d) = %v, %v; want true, nil", durability, published, err)
		}
		assertFile(t, name, "first", 0o666)
		assertOnlyEntry(t, dir, "token")
	}
}

func TestPublishLeavesAnExistingFileAsItWas(t *testing.T) {
	for _, durability := range durabilities {
		dir := t.TempDir()
		name := filepath.Join(dir, "token")
		if err := os.WriteFile(name, []byte("incumbent"), 0o600); err != nil {
			t.Fatal(err)
		}
		published, err := Publish(name, 0o644, durability, writeString("challenger"))
		if err != nil || published {
			t.Fatalf("Publish(%d) over an existing file = %v, %v; want false, nil", durability, published, err)
		}
		assertFile(t, name, "incumbent", 0o600)
		assertOnlyEntry(t, dir, "token")
	}
}

func TestAFailedPublishWriteCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	cause := errors.New("download failed")
	published, err := Publish(filepath.Join(dir, "token"), 0o644, Durable, func(io.Writer) error { return cause })
	if !errors.Is(err, cause) || published {
		t.Fatalf("Publish() = %v, %v; want false and an error wrapping the write's", published, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed publish left %v", entries)
	}
}

func TestPublishIntoAMissingDirectoryIsNotALinkError(t *testing.T) {
	_, err := Publish(filepath.Join(t.TempDir(), "absent", "token"), 0o644, Durable, writeString("x"))
	var linkErr *os.LinkError
	if err == nil || errors.As(err, &linkErr) {
		t.Fatalf("Publish() into a missing dir = %v; want an error that is not an *os.LinkError", err)
	}
}
