package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bmf/links-issue-tracker/internal/doltcli"
)

func TestOpenSyncDoesNotCreateStartupCommitWhenSchemaIsCurrent(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() initial error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() initial error = %v", err)
	}

	repoPath := filepath.Join(doltRoot, "links")
	beforeLog, err := doltcli.Run(ctx, repoPath, "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log before sync open error = %v", err)
	}

	syncStore, err := OpenSync(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := syncStore.Close(); err != nil {
		t.Fatalf("Close() sync error = %v", err)
	}

	afterLog, err := doltcli.Run(ctx, repoPath, "log", "--oneline")
	if err != nil {
		t.Fatalf("dolt log after sync open error = %v", err)
	}

	if countNonEmptyLines(afterLog) != countNonEmptyLines(beforeLog) {
		t.Fatalf("OpenSync() created extra commit:\nbefore:\n%s\nafter:\n%s", beforeLog, afterLog)
	}
}

func TestOpenSyncCreatesDatabaseWhenMissing(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	syncStore, err := OpenSync(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	if err := syncStore.Close(); err != nil {
		t.Fatalf("Close() sync error = %v", err)
	}

	repoPath := filepath.Join(doltRoot, "links")
	status, err := doltcli.Run(ctx, repoPath, "status")
	if err != nil {
		t.Fatalf("dolt status after sync open error = %v", err)
	}
	if !strings.Contains(status, "On branch main") {
		t.Fatalf("unexpected dolt status output after sync open: %q", status)
	}
}

func TestSyncRemoteLifecycle(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() initial error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() initial error = %v", err)
	}

	syncStore, err := OpenSync(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()

	if err := syncStore.SyncAddRemote(ctx, "origin", "https://example.com/repo.git"); err != nil {
		t.Fatalf("SyncAddRemote() error = %v", err)
	}

	remotes, err := syncStore.SyncListRemotes(ctx)
	if err != nil {
		t.Fatalf("SyncListRemotes() after add error = %v", err)
	}
	if len(remotes) != 1 || remotes[0].Name != "origin" {
		t.Fatalf("remotes after add = %#v", remotes)
	}
	if remotes[0].URL == "" {
		t.Fatalf("remotes after add missing URL: %#v", remotes)
	}

	if err := syncStore.SyncRemoveRemote(ctx, "origin"); err != nil {
		t.Fatalf("SyncRemoveRemote() error = %v", err)
	}

	remotes, err = syncStore.SyncListRemotes(ctx)
	if err != nil {
		t.Fatalf("SyncListRemotes() after remove error = %v", err)
	}
	if len(remotes) != 0 {
		t.Fatalf("remotes after remove = %#v, want empty", remotes)
	}
}

func TestSyncRemoteValidation(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	syncStore, err := OpenSync(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenSync() error = %v", err)
	}
	defer syncStore.Close()

	testCases := []struct {
		name    string
		run     func() error
		wantErr string
	}{
		{
			name:    "add remote requires name",
			run:     func() error { return syncStore.SyncAddRemote(ctx, "   ", "https://example.com/repo.git") },
			wantErr: "remote name is required",
		},
		{
			name:    "add remote requires url",
			run:     func() error { return syncStore.SyncAddRemote(ctx, "origin", "   ") },
			wantErr: "remote url is required",
		},
		{
			name:    "remove remote requires name",
			run:     func() error { return syncStore.SyncRemoveRemote(ctx, "   ") },
			wantErr: "remote name is required",
		},
		{
			name:    "fetch requires remote",
			run:     func() error { return syncStore.SyncFetch(ctx, "   ", false) },
			wantErr: "remote is required",
		},
		{
			name: "pull requires remote",
			run: func() error {
				_, err := syncStore.SyncPull(ctx, "   ", "main")
				return err
			},
			wantErr: "remote is required",
		},
		{
			name: "push requires remote",
			run: func() error {
				_, err := syncStore.SyncPush(ctx, "   ", "main", false, false)
				return err
			},
			wantErr: "remote is required",
		},
	}

	for _, tc := range testCases {
		if err := tc.run(); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s error = %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestValidateEmbeddedSyncSupportAcceptsRequiredVersions(t *testing.T) {
	err := validateEmbeddedSyncSupport(map[string]string{
		"github.com/dolthub/dolt/go": minEmbeddedDoltVersion,
		"github.com/dolthub/driver":  minEmbeddedDriverVersion,
	})
	if err != nil {
		t.Fatalf("validateEmbeddedSyncSupport() error = %v", err)
	}
}

func TestValidateEmbeddedSyncSupportRejectsOlderVersions(t *testing.T) {
	err := validateEmbeddedSyncSupport(map[string]string{
		"github.com/dolthub/dolt/go": "v0.40.5-0.20240702155756-bcf4dd5f5cc1",
		"github.com/dolthub/driver":  "v0.2.0",
	})
	if err == nil {
		t.Fatal("validateEmbeddedSyncSupport() error = nil, want version failure")
	}
	if !strings.Contains(err.Error(), "embedded sync requires") {
		t.Fatalf("validateEmbeddedSyncSupport() error = %v, want embedded sync guidance", err)
	}
}
