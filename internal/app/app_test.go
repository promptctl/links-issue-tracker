package app

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

// TestOpenModeContract pins the behavioral split the mode value carries:
// write bootstraps a missing database, read refuses one. [LAW:behavior-not-structure]
func TestOpenModeContract(t *testing.T) {
	cases := []struct {
		name string
		mode AccessMode
		// wantErr is the substring an uninitialized workspace must fail
		// with; empty means open must succeed and bootstrap the database.
		wantErr string
	}{
		{name: "write bootstraps uninitialized workspace", mode: AccessWrite},
		{name: "read refuses uninitialized workspace", mode: AccessRead, wantErr: "not initialized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := gitRepo(t)
			ap, err := Open(context.Background(), repo, tc.mode)
			if tc.wantErr != "" {
				if err == nil {
					ap.Close()
					t.Fatalf("Open(%v) on uninitialized workspace succeeded, want error containing %q", tc.mode, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Open(%v) error = %q, want substring %q", tc.mode, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open(%v) error = %v", tc.mode, err)
			}
			if err := ap.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
		})
	}
}

// TestOpenReadAfterWrite pins that read mode accepts the database write mode
// bootstrapped — the two modes describe one store, not two.
func TestOpenReadAfterWrite(t *testing.T) {
	repo := gitRepo(t)
	writer, err := Open(context.Background(), repo, AccessWrite)
	if err != nil {
		t.Fatalf("Open(write) error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close(write) error = %v", err)
	}
	reader, err := Open(context.Background(), repo, AccessRead)
	if err != nil {
		t.Fatalf("Open(read) after write error = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close(read) error = %v", err)
	}
}

// TestOpenRejectsUnknownMode pins that an unrecognized mode — including the
// zero value — fails closed instead of being granted write access.
// [LAW:no-silent-failure]
func TestOpenRejectsUnknownMode(t *testing.T) {
	repo := gitRepo(t)
	for _, mode := range []AccessMode{"", "admin"} {
		ap, err := Open(context.Background(), repo, mode)
		if err == nil {
			ap.Close()
			t.Fatalf("Open(%q) succeeded, want invalid-mode error", mode)
		}
		if !strings.Contains(err.Error(), "invalid access mode") {
			t.Fatalf("Open(%q) error = %q, want invalid-mode error", mode, err)
		}
	}
}
