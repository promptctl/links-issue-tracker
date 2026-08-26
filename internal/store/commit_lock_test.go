package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAcquireCommitLockNeverEvictsLiveHolderByAge is the regression test for
// the O_EXCL-era eviction bug (links-locking-il18.2): a live owner whose lock
// file looked eleven minutes old was evicted by the mtime threshold, letting a
// second process's mutation walk past it and exit 0. With the lock rebuilt on
// flock, the file's age carries no meaning at all — a backdated lock file with
// a live holder must block a second acquirer until that holder releases, and
// nothing may remove the hold out from under it.
func TestAcquireCommitLockNeverEvictsLiveHolderByAge(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	s := &Store{commitLockPath: lockPath}

	holderRelease, err := acquireStoreLock(context.Background(), lockPath, true, 1, 0)
	if err != nil {
		t.Fatalf("holder acquisition error = %v", err)
	}

	// The measured bug's exact trigger: the lock file looks well past the old
	// ten-minute staleness threshold while its owner is alive.
	backdated := time.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(lockPath, backdated, backdated); err != nil {
		t.Fatalf("Chtimes(lock) error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, err := s.acquireCommitLock(ctx); err == nil {
		t.Fatal("acquireCommitLock() succeeded against a live holder; age-based eviction is back")
	}

	// The contender's failed attempts must not have broken the holder's hold.
	if _, err := acquireStoreLock(context.Background(), lockPath, true, 1, 0); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("probe after failed contender = %v, want ErrWorkspaceBusy (hold intact)", err)
	}

	if err := holderRelease(); err != nil {
		t.Fatalf("holder release error = %v", err)
	}
	lockedCtx, release, err := s.acquireCommitLock(context.Background())
	if err != nil {
		t.Fatalf("acquireCommitLock() after holder release error = %v", err)
	}
	if lockedCtx.Value(commitLockContextKey{}) != true {
		t.Fatal("acquireCommitLock() did not set commit lock context value")
	}
	if err := release(); err != nil {
		t.Fatalf("release error = %v", err)
	}
}

// TestAcquireCommitLockIgnoresDeadResidue pins the other half of the flock
// contract: a leftover lock file with no living holder — an old binary's PID
// payload, an ancient mtime, any content at all — is not a lock. Acquisition
// succeeds immediately with no staleness classification and no reclamation
// step, because absence of a kernel hold IS the death certificate.
func TestAcquireCommitLockIgnoresDeadResidue(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("99999\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}
	ancient := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockPath, ancient, ancient); err != nil {
		t.Fatalf("Chtimes(lock) error = %v", err)
	}
	s := &Store{commitLockPath: lockPath}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, release, err := s.acquireCommitLock(ctx)
	if err != nil {
		t.Fatalf("acquireCommitLock() over dead residue error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release error = %v", err)
	}

	// Release frees the hold for the next acquirer; the file itself persists
	// (an flock release never removes the path, so it cannot delete a lock
	// file a newer holder has already re-locked).
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file should persist after release, stat err = %v", err)
	}
	probeRelease, err := acquireStoreLock(context.Background(), lockPath, true, 1, 0)
	if err != nil {
		t.Fatalf("probe after release error = %v (lock should be free)", err)
	}
	if err := probeRelease(); err != nil {
		t.Fatalf("probe release error = %v", err)
	}
}

// TestWrapCommitLockContention pins the commit lock's contention boundary:
// budget exhaustion (acquireStoreLock's ErrWorkspaceBusy) gains the
// commit-specific operator guidance with the errors.Is discriminator intact,
// and every other failure — a cancellation above all — passes through
// untouched, so "retry after it completes" can never dress an abort. The
// mapping is tested directly rather than by exhausting the real ~15-minute
// budget: the budget is part of the lock's declared identity, not a knob,
// and TestAcquireCommitLockNeverEvictsLiveHolderByAge already proves a live
// holder drives this path's acquisition into contention.
func TestWrapCommitLockContention(t *testing.T) {
	t.Parallel()
	wrapped := wrapCommitLockContention(ErrWorkspaceBusy)
	if !errors.Is(wrapped, ErrWorkspaceBusy) {
		t.Fatalf("wrapped contention lost the ErrWorkspaceBusy discriminator: %v", wrapped)
	}
	if !strings.Contains(wrapped.Error(), "another lit process is writing to this workspace") {
		t.Fatalf("wrapped contention missing operator guidance: %v", wrapped)
	}

	cancellation := context.Canceled
	if got := wrapCommitLockContention(cancellation); got != cancellation {
		t.Fatalf("non-contention error must pass through untouched, got %v", got)
	}
}

// TestSettleCommitLockRelease pins the settle rule for a commit-locked
// operation's release: a release failure joins beside an operation failure
// for diagnosis, but after a durable success it demotes (loud on stderr,
// nil return) — failing a mutation that already landed would invite the
// operator to retry it into a duplicate.
func TestSettleCommitLockRelease(t *testing.T) {
	t.Parallel()
	opErr := errors.New("operation failed")
	relErr := errors.New("release failed")

	if got := SettleCommitLockRelease(nil, nil); got != nil {
		t.Fatalf("settle(nil, nil) = %v, want nil", got)
	}
	if got := SettleCommitLockRelease(opErr, nil); got != opErr {
		t.Fatalf("settle(opErr, nil) = %v, want opErr untouched", got)
	}
	joined := SettleCommitLockRelease(opErr, relErr)
	if !errors.Is(joined, opErr) || !errors.Is(joined, relErr) {
		t.Fatalf("settle(opErr, relErr) = %v, want both joined", joined)
	}
	if got := SettleCommitLockRelease(nil, relErr); got != nil {
		t.Fatalf("settle(nil, relErr) = %v, want nil — a durable success must not report as failure", got)
	}
}

// TestWithMutationResumesAtVersioningAfterStagedCommit pins withMutation's
// phase resume: when the staging transaction has committed and only the
// versioning step (DOLT_COMMIT) fails transiently, the retry must re-run
// versioning alone. Re-running the mutation body against its own staged
// writes would double-apply it — for CreateIssue, the in-tx collision check
// would find the staged row and mint a SECOND issue under a higher nonce —
// so the discriminating assertion is that exactly one issue exists afterward.
func TestWithMutationResumesAtVersioningAfterStagedCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	fires := 0
	st.commitWorkingSetHookForTest = func() error {
		fires++
		if fires == 1 {
			return transientGCContentionError{err: errors.New("simulated: this connection was established when this server performed an online garbage collection. please reconnect.")}
		}
		return nil
	}

	created, err := st.CreateIssue(ctx, CreateIssueInput{Prefix: "test", Title: "survives a versioning retry", Topic: "sync"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if created.ID == "" {
		t.Fatalf("CreateIssue() returned an empty id")
	}
	if fires < 2 {
		t.Fatalf("versioning step ran %d time(s); want the transient failure to have forced a retry", fires)
	}
	st.commitWorkingSetHookForTest = nil

	count, err := st.LocalIssueCount(ctx)
	if err != nil {
		t.Fatalf("LocalIssueCount() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("LocalIssueCount() = %d, want exactly 1 — more means the retry re-ran the staged mutation body", count)
	}
}

// TestCommitWorkingSetOnceRendersStamp pins the commit-stamp flag rendering the
// reconcile's provenance replay depends on: a non-default Author lands verbatim
// as the commit's committer/email (not the session identity), a fixed past Date
// lands to the second, and AllowEmpty lands a commit even on a clean working
// set. The replay tests cannot see a dropped --author on their own — the
// original and the replayed commit share the session identity there — so this
// test stamps an identity the session does not have.
func TestCommitWorkingSetOnceRendersStamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	date := time.Date(2025, 3, 7, 9, 30, 45, 0, time.UTC)
	stamp := commitStamp{
		Message:    "stamped provenance probe",
		Date:       date,
		Author:     "prov-author <prov@example.test>",
		AllowEmpty: true,
	}
	if err := st.commitWorkingSetOnce(ctx, stamp); err != nil {
		t.Fatalf("commitWorkingSetOnce(stamp) error = %v", err)
	}

	var committer, email, message string
	var got time.Time
	err := st.db.QueryRowContext(ctx, `SELECT committer, email, date, message FROM dolt_log('HEAD') LIMIT 1`).Scan(&committer, &email, &got, &message)
	if err != nil {
		t.Fatalf("read stamped head: %v", err)
	}
	if committer != "prov-author" || email != "prov@example.test" {
		t.Fatalf("stamped author = %s <%s>, want prov-author <prov@example.test>", committer, email)
	}
	if !got.UTC().Equal(date) {
		t.Fatalf("stamped date = %s, want %s", got.UTC(), date)
	}
	if message != "stamped provenance probe" {
		t.Fatalf("stamped message = %q, want %q", message, stamp.Message)
	}
}
