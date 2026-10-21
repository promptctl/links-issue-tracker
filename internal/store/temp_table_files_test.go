package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A write open collects the table files a dead writer left staged, and only
// once no writer is live: while one holds the store, the next write open
// cannot take Dolt's LOCK, so it never reaches the collection and the live
// writer's staged file stands. Once that writer is gone, the next write open
// removes the file and leaves everything else in the directory alone. Read
// and sync opens do not look.
func TestAWriteOpenCollectsOnlyWhatADeadWriterStaged(t *testing.T) {
	// serial: no t.Parallel — rewrites the package-level coResidentHolderWait
	// budget, which the refused open below exhausts by design.
	prevBudget := coResidentHolderWait
	coResidentHolderWait = 700 * time.Millisecond
	t.Cleanup(func() { coResidentHolderWait = prevBudget })

	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	writer, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got := writer.Migrated().TempTableFiles; got == nil || *got != (storage.TempTableFilesCollection{}) {
		t.Errorf("a fresh store's open collected %+v, want it to have looked and found nothing", got)
	}
	dir := TempTableFilesDir(doltRoot)
	staged := filepath.Join(dir, "buffered_file_byte_sink_4242")
	writeFileForTest(t, staged, strings.Repeat("x", 1234))

	if refused, err := Open(ctx, doltRoot, "test-workspace-id"); !errors.Is(err, ErrWorkspaceBusy) {
		if refused != nil {
			_ = refused.Close()
		}
		_ = writer.Close()
		t.Fatalf("a second write open beside a live writer: error = %v, want ErrWorkspaceBusy", err)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Errorf("the live writer's staged file after a refused open: %v, want it standing", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	spared := []string{
		filepath.Join(dir, "dolt_embedded_metrics"),
		filepath.Join(dir, "someone-elses.tmp"),
	}
	for _, path := range spared {
		writeFileForTest(t, path, "")
	}
	stagedNamedDir := filepath.Join(dir, "buffered_file_byte_sink_7")
	if err := os.Mkdir(stagedNamedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	spared = append(spared, stagedNamedDir)

	next, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() after the writer closed: error = %v", err)
	}
	t.Cleanup(func() { _ = next.Close() })
	want := storage.TempTableFilesCollection{Files: 1, Bytes: 1234}
	if got := next.Migrated().TempTableFiles; got == nil || *got != want {
		t.Errorf("Migrated().TempTableFiles = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dead writer's staged file: stat error = %v, want it removed", err)
	}
	for _, path := range spared {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: %v, want it spared", path, err)
		}
	}
	if err := next.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reader, err := OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v", err)
	}
	if got := reader.Migrated().TempTableFiles; got != nil {
		t.Errorf("a read open's TempTableFiles = %+v, want nil: it does not look", got)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("reader Close() error = %v", err)
	}
	syncer, err := OpenSync(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if got := syncer.Migrated().TempTableFiles; got != nil {
		t.Errorf("a sync open's TempTableFiles = %+v, want nil: it does not look", got)
	}
	if err := syncer.Close(); err != nil {
		t.Fatalf("syncer Close() error = %v", err)
	}
}

// A collection that cannot read the staging directory leaves the open
// standing and reports why on what the open did.
func TestAWriteOpenWhoseCollectionFailsStillOpens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	first, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	dir := TempTableFilesDir(doltRoot)
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	s, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() over an unreadable staging dir: error = %v, want the open to stand", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got := s.Migrated()
	if got.TempTableFiles == nil || !errors.Is(got.TempTableFiles.Err, os.ErrPermission) {
		t.Fatalf("Migrated().TempTableFiles = %+v, want the permission failure", got.TempTableFiles)
	}
	if !errors.Is(got.Failures(), os.ErrPermission) {
		t.Errorf("Migrated().Failures() = %v, want it to carry the collection's failure", got.Failures())
	}
}

// Only Dolt's exact staged-table-file name is collectable: its prefix and a
// decimal number.
func TestIsStagedTableFileName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"buffered_file_byte_sink_123":   true,
		"buffered_file_byte_sink_0":     true,
		"buffered_file_byte_sink_":      false,
		"buffered_file_byte_sink_12a":   false,
		"buffered_file_byte_sink_-1":    false,
		"xbuffered_file_byte_sink_1":    false,
		"buffered_file_byte_sink_1.tmp": false,
		"dolt_embedded_metrics":         false,
		"":                              false,
	} {
		if got := isStagedTableFileName(name); got != want {
			t.Errorf("isStagedTableFileName(%q) = %v, want %v", name, got, want)
		}
	}
}

func writeFileForTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
