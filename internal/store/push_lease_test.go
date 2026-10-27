package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// TestLeasedPushLandsOnlyOverTheHeadItNames drives two checkouts sharing one
// remote. bravo pushes after alpha last read the remote; alpha then rewrites
// its own history and pushes it under a lease. Naming the head alpha last saw
// is refused and bravo's push stays; naming the remote's actual head lands a
// non-fast-forward rewrite; and the same push again, as a transport retry
// would send it, still lands.
func TestLeasedPushLandsOnlyOverTheHeadItNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	alpha, bravo := migratedDoltDir(t), migratedDoltDir(t)
	epic := seedReconcileRemote(t, ctx, alpha, remoteURL)
	createAndPush(t, ctx, alpha, epic, "a1")
	createAndPush(t, ctx, alpha, epic, "a2")
	seen := headAt(t, ctx, alpha)
	adoptRemote(t, ctx, bravo, remoteURL)
	createAndPush(t, ctx, bravo, epic, "b1")
	bravoHead := headAt(t, ctx, bravo)

	// alpha's rewrite: its history re-rooted below its head, so the head it
	// pushes descends from neither the head it saw nor bravo's.
	var cut string
	withChunkStore(t, alpha, func(ddb *doltdb.DoltDB) {
		cut = firstParentBelowHead(t, ctx, ddb, 1).String()
	})
	rewritten, err := ReRoot(ctx, testStderr(), alpha, "master", cut)
	if err != nil {
		t.Fatalf("ReRoot(alpha) error = %v", err)
	}

	st := openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	push := func(expected string) storage.LeaseOutcome {
		t.Helper()
		var pushed storage.LeaseOutcome
		err := st.runSyncMutation(ctx, func(ctx context.Context) error {
			var err error
			pushed, err = st.pushLeasedWithinLock(ctx, remoteName(t, "origin"), "master", "master", expected)
			return err
		})
		if err != nil {
			t.Fatalf("leased push expecting %s error = %v", expected, err)
		}
		return pushed
	}
	remoteHead := func() string {
		t.Helper()
		if err := st.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
			t.Fatalf("SyncFetch(alpha) error = %v", err)
		}
		head, err := commitHashOfRef(ctx, st.db, "remotes/origin/master")
		if err != nil {
			t.Fatal(err)
		}
		return head
	}

	if stale := push(seen); stale != storage.LeaseStale {
		t.Fatalf("lease on the head alpha last saw = %q, want %q", stale, storage.LeaseStale)
	}
	if got := remoteHead(); got != bravoHead {
		t.Fatalf("after a stale lease the remote is at %s, want bravo's push %s", got, bravoHead)
	}

	if held := push(bravoHead); held != storage.LeaseHeld {
		t.Fatalf("lease on the remote's head = %q, want %q", held, storage.LeaseHeld)
	}
	if got := remoteHead(); got != rewritten.Head {
		t.Fatalf("after a held lease the remote is at %s, want alpha's rewritten head %s", got, rewritten.Head)
	}

	if again := push(bravoHead); again != storage.LeaseHeld {
		t.Fatalf("the same leased push again = %q, want it held over its own landed head", again)
	}

	// A remote that cannot be opened gives no verdict on the lease, so it is
	// reported as the failure it is: not a stale outcome, not a refusal, and
	// not the transport giving out. A file remote is created wherever it can
	// be, so this one's directory becomes a regular file.
	gone := filepath.Join(t.TempDir(), "remote")
	if err := st.SyncAddRemote(ctx, remoteName(t, "gone"), "file://"+gone); err != nil {
		t.Fatalf("SyncAddRemote(gone) error = %v", err)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gone, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = st.runSyncMutation(ctx, func(ctx context.Context) error {
		unreached, err := st.pushLeasedWithinLock(ctx, remoteName(t, "gone"), "master", "master", bravoHead)
		if err == nil || errors.As(err, new(pushRefusedError)) || errors.As(err, new(RemoteUnreachableError)) || !strings.Contains(err.Error(), actions.ErrFailedToGetRemoteDb.Error()) {
			t.Errorf("leased push to a remote that cannot be opened = %q, %v; want Dolt's %q, neither a refusal nor unreachable", unreached, err, actions.ErrFailedToGetRemoteDb)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestLeasedPushRefusesToReplaceASchemaAheadHead names, as the lease's
// expected head, a remote head at a schema above this binary's, while the
// pusher's tracking ref still lags at the older schema. The lease would hold,
// so the push is refused before it is sent and the remote keeps its head.
func TestLeasedPushRefusesToReplaceASchemaAheadHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")
	alpha, bravo := migratedDoltDir(t), migratedDoltDir(t)
	seedReconcileRemote(t, ctx, alpha, remoteURL)
	adoptRemote(t, ctx, bravo, remoteURL)

	// bravo, as a newer binary would, advances the remote's schema.
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatal(err)
	}
	newer, err := Open(ctx, testEnv(), testStderr(), bravo, "ws")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newer.db.ExecContext(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, registryMax+1); err != nil {
		t.Fatal(err)
	}
	if err := newer.commitWorkingSet(ctx, "a newer binary's schema"); err != nil {
		t.Fatal(err)
	}
	if err := newer.Close(); err != nil {
		t.Fatal(err)
	}
	pusher := openSyncOrFatal(t, ctx, bravo)
	if _, err := pusher.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
		t.Fatalf("SyncPush(bravo) error = %v", err)
	}
	if err := pusher.Close(); err != nil {
		t.Fatal(err)
	}
	ahead := headAt(t, ctx, bravo)

	// alpha reads the remote's head through a second name for it, so the
	// head is in alpha's store while remotes/origin/master still lags.
	st := openSyncOrFatal(t, ctx, alpha)
	defer func() { _ = st.Close() }()
	if err := st.SyncAddRemote(ctx, remoteName(t, "mirror"), remoteURL); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncFetch(ctx, remoteName(t, "mirror")); err != nil {
		t.Fatal(err)
	}
	err = st.runSyncMutation(ctx, func(ctx context.Context) error {
		_, err := st.pushLeasedWithinLock(ctx, remoteName(t, "origin"), "master", "master", ahead)
		return err
	})
	if !errors.As(err, new(*RemoteSchemaAheadError)) {
		t.Fatalf("leased push over a schema-ahead head = %v, want *RemoteSchemaAheadError", err)
	}
	if err := st.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatal(err)
	}
	if got, err := commitHashOfRef(ctx, st.db, "remotes/origin/master"); err != nil || got != ahead {
		t.Fatalf("remote head after the refusal = %s, %v; want the schema-ahead head %s", got, err, ahead)
	}
}
