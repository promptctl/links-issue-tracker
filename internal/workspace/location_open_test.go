package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestLocationFromStorageDirMatchesDeriveLocation locks the geometry seam: a
// Location rebuilt from a StorageDir string (what a caller holds after reading a
// `lit stores` line) must be byte-identical to the Location deriveLocation mints
// for the same repository. If the two ever drift, `lit ls-at <dir>` would open a
// different store than the one `lit stores` reported.
func TestLocationFromStorageDirMatchesDeriveLocation(t *testing.T) {
	repo := mkdir(t, filepath.Join(t.TempDir(), "repo"))
	run(t, repo, "git", "init")

	derived, err := deriveLocation(repo)
	if err != nil {
		t.Fatalf("deriveLocation() error = %v", err)
	}
	rebuilt := LocationFromStorageDir(derived.StorageDir)
	if rebuilt != derived {
		t.Fatalf("LocationFromStorageDir(%q) = %+v, want %+v", derived.StorageDir, rebuilt, derived)
	}
}

// TestReadConfigReturnsIdentityWithoutWriting is the read-only guarantee the
// cross-project open depends on: reading a foreign store's config must surface
// its workspace_id and must not create, modify, or touch the file. The test
// asserts both the value and that the on-disk bytes are unchanged.
func TestReadConfigReturnsIdentityWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"workspace_id":"ws-abc","issue_prefix":"proj","schema_version":1}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	if cfg.WorkspaceID != "ws-abc" {
		t.Fatalf("ReadConfig().WorkspaceID = %q, want %q", cfg.WorkspaceID, "ws-abc")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(after) error = %v", err)
	}
	if string(after) != string(original) {
		t.Fatalf("ReadConfig mutated config.json:\n got=%q\nwant=%q", after, original)
	}
}

// TestReadConfigMissingFilePreservesNotExist confirms a missing config surfaces
// an error that still satisfies errors.Is(os.ErrNotExist) — the property
// loadOrCreateConfig relies on to tell "no config yet, create one" from a real
// failure after it was refactored to read through ReadConfig.
func TestReadConfigMissingFilePreservesNotExist(t *testing.T) {
	_, err := ReadConfig(filepath.Join(t.TempDir(), "absent", "config.json"))
	if err == nil {
		t.Fatalf("ReadConfig(missing) returned nil error, want a not-exist error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadConfig(missing) error = %v, want errors.Is(os.ErrNotExist)", err)
	}
}

// TestReadConfigRejectsMissingWorkspaceID guards against opening a foreign store
// with an empty identity: a config without workspace_id is a loud error, never a
// silently-blank id handed to OpenForRead.
func TestReadConfigRejectsMissingWorkspaceID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"issue_prefix":"proj"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	if _, err := ReadConfig(path); err == nil {
		t.Fatalf("ReadConfig(no workspace_id) returned nil error, want a surfaced failure")
	}
}
