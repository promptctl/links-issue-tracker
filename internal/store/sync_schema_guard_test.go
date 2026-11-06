package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// seedFutureSchemaRemote seeds a remote whose head records a goose schema version
// ABOVE this binary's registry max — the shape a NEWER binary leaves behind after
// advancing the shared remote. It stamps a synthetic future goose_db_version row
// and pushes; the first push is to an empty remote, so the schema guard is a no-op
// and the bump lands. Returns the seeded issue id and the future version. This
// binary cannot itself produce that version — which is exactly the state the
// schema guard must refuse to write over.
func seedFutureSchemaRemote(t *testing.T, ctx context.Context, root, remoteURL string) (string, int64) {
	t.Helper()
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion: %v", err)
	}
	future := registryMax + 1

	st, err := Open(ctx, testEnv(), testStderr(), root, "ws")
	if err != nil {
		t.Fatalf("Open(seed %s): %v", root, err)
	}
	epic, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "seed epic", Topic: "topic", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(seed epic): %v", err)
	}
	issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "seed", Topic: "topic", IssueType: "task", ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue(seed): %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, future); err != nil {
		t.Fatalf("insert synthetic future goose row: %v", err)
	}
	if err := st.commitWorkingSet(ctx, "seed: synthetic future schema marker"); err != nil {
		t.Fatalf("commit future marker: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(seed): %v", err)
	}

	// The newer binary's release carries the schema it pushes.
	sync, err := OpenSync(ctx, testEnv().WithReleasedSchema(ceilingAt(future)), testStderr(), root, "ws")
	if err != nil {
		t.Fatalf("OpenSync(seed): %v", err)
	}
	if err := sync.SyncAddRemote(ctx, remoteName(t, "origin"), remoteURL); err != nil {
		t.Fatalf("SyncAddRemote(seed): %v", err)
	}
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", true, false); err != nil {
		t.Fatalf("SyncPush(seed): %v", err)
	}
	if err := sync.Close(); err != nil {
		t.Fatalf("Close(seed sync): %v", err)
	}
	return issue.ID, future
}

// TestRemoteHeadSchemaReadsVersion proves the read primitive returns the remote
// head's applied schema version as raw data, AS OF the commit hash — no branch
// move, no lift.
func TestRemoteHeadSchemaReadsVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	_, future := seedFutureSchemaRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch: %v", err)
	}
	head, synced, err := syncB.trackingHeadHash(ctx, remoteName(t, "origin"), "master")
	if err != nil || !synced {
		t.Fatalf("trackingHeadHash: synced=%v err=%v", synced, err)
	}
	version, err := syncB.remoteHeadSchema(ctx, head)
	if err != nil {
		t.Fatalf("remoteHeadSchema: %v", err)
	}
	if version != future {
		t.Fatalf("remote head version = %d, want %d", version, future)
	}
}

// TestGuardRemoteSchemaAheadDetects proves the guard returns the typed refusal
// carrying the versions when the remote head is ahead.
func TestGuardRemoteSchemaAheadDetects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")

	_, future := seedFutureSchemaRemote(t, ctx, rootA, remoteURL)
	registryMax, _ := migrations.MaxVersion()
	adoptRemote(t, ctx, rootB, remoteURL)

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch: %v", err)
	}
	err := syncB.refuseTrackingHeadAhead(ctx, remoteName(t, "origin"), "master")
	var ahead *RemoteSchemaAheadError
	if !errors.As(err, &ahead) {
		t.Fatalf("refuseSchemaAhead(tracking head) = %v, want *RemoteSchemaAheadError", err)
	}
	if ahead.RemoteVersion != future || ahead.BinarySupportedMax != registryMax {
		t.Fatalf("refusal = %+v, want RemoteVersion=%d Max=%d", ahead, future, registryMax)
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
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch: %v", err)
	}
	if err := syncB.refuseTrackingHeadAhead(ctx, remoteName(t, "origin"), "master"); err != nil {
		t.Fatalf("guard at-or-below max = %v, want nil", err)
	}
	// A branch the remote has never seen has no tracking ref, so the guard cannot
	// and must not block it.
	if err := syncB.refuseTrackingHeadAhead(ctx, remoteName(t, "origin"), "no-such-branch"); err != nil {
		t.Fatalf("guard never-synced branch = %v, want nil", err)
	}
}

// refuseTrackingHeadAhead is the schema-ahead verdict on a branch's tracking
// head: the reconcile's refusal (refuseSchemaAhead) judged off the read of R
// a push makes, so the detection tests judge the one verdict on a fetched
// head without driving a divergence.
func (s *Store) refuseTrackingHeadAhead(ctx context.Context, remote storage.RemoteName, branch string) error {
	remoteHead, err := s.trackingHeadSchema(ctx, remote, branch)
	if err != nil {
		return err
	}
	return refuseSchemaAhead(remote, branch, remoteHead)
}

// TestAPushAtTheRemotesOwnSchemaLands is the push-path acceptance of the
// decision in design-docs/schema-reversibility.md ("A push at the remote's
// own schema lands"): a checkout that adopted a remote head at a schema above
// this binary's, and wrote under it as the open admits, pushes. A fresh clone
// sees the edit, and the remote head's schema after the push is still the
// ahead version: a commit at R is no regression. This binary's released
// ceiling (testEnv: the registry max) is below R, so the landing also pins
// that the ceiling judges a raise only.
func TestAPushAtTheRemotesOwnSchemaLands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	rootC := filepath.Join(base, "c")
	remoteURL := "file://" + filepath.Join(base, "remote")

	id, future := seedFutureSchemaRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)
	// B's open admits the write: the store is ahead of the binary with its
	// baseline intact and no minimum writer schema above the binary's.
	updateLocal(t, ctx, rootB, id, storage.UpdateIssueInput{Lane: strptr("from-b")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch: %v", err)
	}
	pushed, err := syncB.SyncPush(ctx, remoteName(t, "origin"), "master", false, false)
	if err != nil {
		t.Fatalf("SyncPush at the remote's own schema = %v, want the push to land", err)
	}
	if head := trackingSchema(t, ctx, syncB); head.head != pushed.Head || head.version != future {
		t.Fatalf("remote head after the push = %+v, want the pushed head %s at schema %d", head, pushed.Head, future)
	}
	// syncB is done with its work; close before opening rootC below and hold
	// nothing open past this point.
	if err := syncB.Close(); err != nil {
		t.Fatalf("syncB.Close(): %v", err)
	}

	// A fresh clone sees B's edit.
	adoptRemote(t, ctx, rootC, remoteURL)
	stC, err := Open(ctx, testEnv(), testStderr(), rootC, "ws")
	if err != nil {
		t.Fatalf("Open(C): %v", err)
	}
	defer stC.Close()
	if got := getIssueOrFatal(t, ctx, stC, id); got.Lane != "from-b" {
		t.Fatalf("B's edit did not reach the remote (lane=%q, want %q)", got.Lane, "from-b")
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
	updateLocal(t, ctx, rootB, id, storage.UpdateIssueInput{Lane: strptr("from-b")}) // B: 1 ahead

	// A advances the remote divergently AND bumps it to a future schema in the same
	// push. The push runs while the remote is still at the current schema, so the
	// guard does not block the bump itself; only clones that fetch it are refused.
	advanceRemoteToFutureSchema(t, ctx, rootA, id, storage.UpdateIssueInput{Lane: strptr("from-a")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	if err := syncB.SyncFetch(ctx, remoteName(t, "origin")); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	fresh, err := syncB.SyncFreshness(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncFreshness: %v", err)
	}
	if fresh.State() != storage.SyncDiverged {
		t.Fatalf("pre-reconcile state = %v, want diverged", fresh.State())
	}
	localHeadBefore := headCommit(t, ctx, syncB)

	res, err := syncB.SyncReconcile(ctx, remoteName(t, "origin"), "master")
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
// future goose schema version, and pushes — all while the remote is still at the
// current schema, so the push is not self-blocked. The remote head then sits at a
// schema this binary cannot produce.
func advanceRemoteToFutureSchema(t *testing.T, ctx context.Context, root, id string, in storage.UpdateIssueInput) {
	t.Helper()
	updateLocal(t, ctx, root, id, in) // commits the divergent edit
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion: %v", err)
	}
	st, err := Open(ctx, testEnv(), testStderr(), root, "ws")
	if err != nil {
		t.Fatalf("Open(advance %s): %v", root, err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, registryMax+1); err != nil {
		t.Fatalf("insert synthetic future goose row: %v", err)
	}
	if err := st.commitWorkingSet(ctx, "advance: synthetic future schema marker"); err != nil {
		t.Fatalf("commit future marker: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(advance): %v", err)
	}
	// The newer binary's release carries the schema it pushes.
	sync, err := OpenSync(ctx, testEnv().WithReleasedSchema(ceilingAt(registryMax+1)), testStderr(), root, "ws")
	if err != nil {
		t.Fatalf("OpenSync(advance): %v", err)
	}
	if _, err := sync.SyncPush(ctx, remoteName(t, "origin"), "master", false, false); err != nil {
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

// TestRemoteSchemaAheadErrorStatesTheRequirement pins that the refusal tells an
// operator to install a lit supporting the remote's schema version, and never
// names a build identity to install.
func TestRemoteSchemaAheadErrorStatesTheRequirement(t *testing.T) {
	t.Parallel()
	msg := (&RemoteSchemaAheadError{
		Remote: "origin", Branch: "master",
		RemoteVersion: 5, BinarySupportedMax: 4,
	}).Error()

	for _, want := range []string{"origin/master", "schema version 5", "up to 4", "lit upgrade", "supports schema version 5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "--to") {
		t.Errorf("refusal named a build to install by identity:\n%s", msg)
	}
}

// TestABehindLandingAdoptsAnAheadHeadAndReportsIt pins the decision on the
// receive's landing: a clone that is merely behind a remote whose head is at
// a schema above this binary lands that head (the landing authors no commit,
// so there is nothing for the schema-ahead refusal to guard; the open decides
// what this binary may do with the store it finds), and every settle reports
// the remote head's schema against this binary as a value, whatever state it
// ended in: the behind landing, the up-to-date settle after it, and the
// reconcile's not-diverged answer. A surface that ends the remote-schema
// episode reads that value instead of inferring it from the state.
func TestABehindLandingAdoptsAnAheadHeadAndReportsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	registryMax, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion: %v", err)
	}

	// The remote starts at the current schema; B adopts it and writes nothing.
	id := seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)
	// A advances the remote and raises it to a future schema; B is now behind.
	advanceRemoteToFutureSchema(t, ctx, rootA, id, storage.UpdateIssueInput{Lane: strptr("from-a")})

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	origin := remoteName(t, "origin")
	want := storage.RemoteSchemaAhead{RemoteSchema: registryMax + 1, BinarySupportedMax: registryMax}

	landed, err := syncB.SyncReceive(ctx, origin, "master")
	if err != nil {
		t.Fatalf("SyncReceive(B behind an ahead remote) error = %v, want the landing", err)
	}
	if landed.State != storage.SyncReceiveFastForwarded || landed.RemoteSchemaAhead != want {
		t.Fatalf("SyncReceive = %+v, want fast_forwarded reporting %+v", landed, want)
	}
	tracking, err := commitHashOfRef(ctx, syncB.db, "remotes/origin/master")
	if err != nil {
		t.Fatalf("tracking head: %v", err)
	}
	if head := headCommit(t, ctx, syncB); head != tracking {
		t.Fatalf("local head %q after the landing, want the remote head %q", head, tracking)
	}

	// Sitting on the head, the settle still reads and reports it.
	settled, err := syncB.SyncReceive(ctx, origin, "master")
	if err != nil {
		t.Fatalf("SyncReceive(B up to date) error = %v", err)
	}
	if settled.State != storage.SyncReceiveUpToDate || settled.RemoteSchemaAhead != want {
		t.Fatalf("SyncReceive = %+v, want up_to_date reporting %+v", settled, want)
	}
	// And so does a reconcile that finds nothing to reconcile.
	rec, err := syncB.SyncReconcile(ctx, origin, "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B not diverged) error = %v", err)
	}
	if rec.State != storage.SyncReconcileNotDiverged || rec.RemoteSchemaAhead != want {
		t.Fatalf("SyncReconcile = %+v, want not_diverged reporting %+v", rec, want)
	}
}

// TestASettleReportsAHeadWithinSupportAsNotAhead is the other half of the
// read: against a remote at this binary's own schema the settle reports the
// zero value, the one that ends a remote-schema episode.
func TestASettleReportsAHeadWithinSupportAsNotAhead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(base, "remote")
	seedReconcileRemote(t, ctx, rootA, remoteURL)
	adoptRemote(t, ctx, rootB, remoteURL)

	syncB := openSyncOrFatal(t, ctx, rootB)
	defer syncB.Close()
	settled, err := syncB.SyncReceive(ctx, remoteName(t, "origin"), "master")
	if err != nil {
		t.Fatalf("SyncReceive error = %v", err)
	}
	if settled.RemoteSchemaAhead != (storage.RemoteSchemaAhead{}) || settled.RemoteSchemaAhead.Ahead() {
		t.Fatalf("a remote at this binary's schema reported as ahead: %+v", settled.RemoteSchemaAhead)
	}
}
