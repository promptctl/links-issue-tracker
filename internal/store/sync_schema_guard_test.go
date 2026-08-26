package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// seedFutureSchemaRemote seeds a remote whose head records a goose schema version
// ABOVE this binary's registry max — the shape a NEWER binary leaves behind after
// advancing the shared remote. It stamps a synthetic future goose_db_version row
// (and, when producer != "", a producer binary version) and pushes; the first push
// is to an empty remote, so the schema guard is a no-op and the bump lands. Returns
// the seeded issue id and the future version. This binary cannot itself produce
// that version — which is exactly the state links-sync-7p7q.4 must refuse to write
// over.
func seedFutureSchemaRemote(t *testing.T, ctx context.Context, root, remoteURL, producer string) (string, int64) {
	t.Helper()
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion: %v", err)
	}
	future := registryMax + 1

	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(seed %s): %v", root, err)
	}
	issue, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "seed", Topic: "topic", IssueType: "task"})
	if err != nil {
		t.Fatalf("CreateIssue(seed): %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, future); err != nil {
		t.Fatalf("insert synthetic future goose row: %v", err)
	}
	if producer != "" {
		if err := st.setMeta(ctx, nil, producerBinaryVersionMetaKey, producer); err != nil {
			t.Fatalf("stamp producer: %v", err)
		}
	}
	if err := st.commitWorkingSet(ctx, "seed: synthetic future schema marker"); err != nil {
		t.Fatalf("commit future marker: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(seed): %v", err)
	}

	sync := openSyncOrFatal(t, ctx, root)
	if err := sync.SyncAddRemote(ctx, "origin", remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(seed): %v", err)
	}
	if _, err := sync.SyncPush(ctx, "origin", "master", true, false); err != nil {
		t.Fatalf("SyncPush(seed): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(seed sync): %v", err)
	}
	return issue.ID, future
}

// TestRemoteHeadSchemaReadsVersionAndProducer proves the read primitive returns the
// remote head's applied schema version and producer stamp as raw data, AS OF the
// commit hash — no branch move, no lift.
func TestRemoteHeadSchemaReadsVersionAndProducer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	_, future := seedFutureSchemaRemote(t, ctx, rootA, remoteURL, "v9.9.0")
	adoptRemote(t, ctx, rootB, remoteURL)

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch: %v", err)
	}
	head, synced, err := syncB.trackingHeadHash(ctx, "origin", "master")
	if err != nil || !synced {
		t.Fatalf("trackingHeadHash: synced=%v err=%v", synced, err)
	}
	version, producer, err := syncB.remoteHeadSchema(ctx, head)
	if err != nil {
		t.Fatalf("remoteHeadSchema: %v", err)
	}
	if version != future {
		t.Fatalf("remote head version = %d, want %d", version, future)
	}
	if producer != "v9.9.0" {
		t.Fatalf("remote head producer = %q, want v9.9.0", producer)
	}
}

// TestGuardRemoteSchemaAheadDetects proves the guard returns the typed refusal
// carrying the versions and producer when the remote head is ahead.
func TestGuardRemoteSchemaAheadDetects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	_, future := seedFutureSchemaRemote(t, ctx, rootA, remoteURL, "v9.9.0")
	registryMax, _ := migrations.MaxVersion()
	adoptRemote(t, ctx, rootB, remoteURL)

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch: %v", err)
	}
	err := syncB.guardRemoteSchemaAhead(ctx, "origin", "master")
	var ahead *RemoteSchemaAheadError
	if !errors.As(err, &ahead) {
		t.Fatalf("guardRemoteSchemaAhead = %v, want *RemoteSchemaAheadError", err)
	}
	if ahead.RemoteVersion != future || ahead.BinarySupportedMax != registryMax || ahead.RemoteProducerVersion != "v9.9.0" {
		t.Fatalf("refusal = %+v, want RemoteVersion=%d Max=%d Producer=v9.9.0", ahead, future, registryMax)
	}
}

// TestGuardRemoteSchemaNotAheadAtOrBelowMax proves the guard is a no-op when the
// remote head is at or below this binary's registry max — so the existing
// same-schema and old-schema sync paths are not falsely blocked. A never-synced
// branch (no tracking ref) is likewise a no-op.
func TestGuardRemoteSchemaNotAheadAtOrBelowMax(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	seedReconcileRemote(t, ctx, rootA, remoteURL) // remote at registry max
	adoptRemote(t, ctx, rootB, remoteURL)

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch: %v", err)
	}
	if err := syncB.guardRemoteSchemaAhead(ctx, "origin", "master"); err != nil {
		t.Fatalf("guard at-or-below max = %v, want nil", err)
	}
	// A branch the remote has never seen has no tracking ref, so the guard cannot
	// and must not block it.
	if err := syncB.guardRemoteSchemaAhead(ctx, "origin", "no-such-branch"); err != nil {
		t.Fatalf("guard never-synced branch = %v, want nil", err)
	}
}

// TestSyncPushRefusesWhenRemoteSchemaAhead is the push-path acceptance: a clone at
// max M pushing onto a remote whose head is at schema N>M is refused with the typed
// error and ZERO commits reach the remote — a fresh clone still sees the pre-push
// state, so no --force-style regression happened.
func TestSyncPushRefusesWhenRemoteSchemaAhead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	rootC := filepath.Join(base, "c")
	remoteURL := "file://" + filepath.Join(base, "remote")

	id, _ := seedFutureSchemaRemote(t, ctx, rootA, remoteURL, "v9.9.0")
	adoptRemote(t, ctx, rootB, remoteURL)
	// B edits locally — a would-be fast-forward push onto the remote.
	updateLocal(t, ctx, rootB, id, UpdateIssueInput{Lane: strptr("from-b")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch: %v", err)
	}
	_, err := syncB.SyncPush(ctx, "origin", "master", false, false)
	var ahead *RemoteSchemaAheadError
	if !errors.As(err, &ahead) {
		t.Fatalf("SyncPush onto ahead remote = %v, want *RemoteSchemaAheadError", err)
	}
	// syncB is done with its work; close before opening rootC below and hold
	// nothing open past this point. (rootB comes from unrelatedDoltDir with
	// its own t.TempDir parent, so the two stores' workspace/commit lock
	// paths are independent — the early close is hygiene, not a lock-sharing
	// requirement.)
	if err := syncB.Close(); err != nil {
		t.Fatalf("syncB.Close(): %v", err)
	}

	// Property: zero commits reached the remote. A fresh clone still sees the seed
	// state, without B's edit.
	adoptRemote(t, ctx, rootC, remoteURL)
	stC, err := Open(ctx, rootC, "ws")
	if err != nil {
		t.Fatalf("Open(C): %v", err)
	}
	defer stC.Close()
	got := getIssueOrFatal(t, ctx, stC, id)
	if got.Lane == "from-b" {
		t.Fatalf("B's edit reached the remote despite the schema-ahead refusal (lane=%q)", got.Lane)
	}
}

// TestSyncReconcileRefusesWhenRemoteSchemaAhead is the reconcile-path acceptance:
// a diverged clone at max M reconciling against a remote head at schema N>M is
// refused with the typed error BEFORE any write — its local head is untouched (no
// regressed replay commit) and the remote is unchanged.
func TestSyncReconcileRefusesWhenRemoteSchemaAhead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	// Remote starts at the CURRENT schema so both sides can build a real divergence.
	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)
	updateLocal(t, ctx, rootB, id, UpdateIssueInput{Lane: strptr("from-b")}) // B: 1 ahead

	// A advances the remote divergently AND bumps it to a future schema in the same
	// push. The push runs while the remote is still at the current schema, so the
	// guard does not block the bump itself; only clones that fetch it are refused.
	advanceRemoteToFutureSchema(t, ctx, rootA, id, "v9.9.0", UpdateIssueInput{Lane: strptr("from-a")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	fresh, err := syncB.SyncFreshness(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncFreshness: %v", err)
	}
	if fresh.State() != SyncDiverged {
		t.Fatalf("pre-reconcile state = %v, want diverged", fresh.State())
	}
	localHeadBefore := headCommit(t, ctx, syncB)

	res, err := syncB.SyncReconcile(ctx, "origin", "master")
	var ahead *RemoteSchemaAheadError
	if !errors.As(err, &ahead) {
		t.Fatalf("SyncReconcile onto ahead remote = %v (state %q), want *RemoteSchemaAheadError", err, res.State)
	}
	// Zero write: the local head did not move (no regressed replay commit landed),
	// and no recovery snapshot was taken.
	if after := headCommit(t, ctx, syncB); after != localHeadBefore {
		t.Fatalf("reconcile moved the local head despite refusing: %q -> %q", localHeadBefore, after)
	}
	assertWorkingSetClean(t, ctx, syncB)
	assertScratchBranchCleanedUp(t, ctx, syncB)
}

// advanceRemoteToFutureSchema applies a field edit at root, stamps a synthetic
// future goose schema version plus a producer, and pushes — all while the remote is
// still at the current schema, so the push is not self-blocked. The remote head then
// sits at a schema this binary cannot produce.
func advanceRemoteToFutureSchema(t *testing.T, ctx context.Context, root, id, producer string, in UpdateIssueInput) {
	t.Helper()
	updateLocal(t, ctx, root, id, in) // commits the divergent edit
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion: %v", err)
	}
	st, err := Open(ctx, root, "ws")
	if err != nil {
		t.Fatalf("Open(advance %s): %v", root, err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, registryMax+1); err != nil {
		t.Fatalf("insert synthetic future goose row: %v", err)
	}
	if producer != "" {
		if err := st.setMeta(ctx, nil, producerBinaryVersionMetaKey, producer); err != nil {
			t.Fatalf("stamp producer: %v", err)
		}
	}
	if err := st.commitWorkingSet(ctx, "advance: synthetic future schema marker"); err != nil {
		t.Fatalf("commit future marker: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(advance): %v", err)
	}
	sync := openSyncOrFatal(t, ctx, root)
	if _, err := sync.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(advance): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(advance push): %v", err)
	}
}

func TestIsDoltCommitHash(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"ou5cgn2ialas6nfhb0eo6avngkjblh0v", true}, // real 32-char base32 hash
		{"00000000000000000000000000000000", true},
		{"ou5cgn2ialas6nfhb0eo6avngkjblh0", false},   // 31 chars
		{"ou5cgn2ialas6nfhb0eo6avngkjblh0vv", false}, // 33 chars
		{"ou5cgn2ialas6nfhb0eo6avngkjblh0w", false},  // 'w' is outside base32
		{"OU5CGN2IALAS6NFHB0EO6AVNGKJBLH0V", false},  // uppercase not produced by Dolt
		{"", false},
	}
	for _, c := range cases {
		if got := isDoltCommitHash(c.in); got != c.want {
			t.Errorf("isDoltCommitHash(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
