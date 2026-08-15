package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemoveStaleCommitLockRemovesDeadOwnerImmediately(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("12345\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	originalProbe := commitLockPIDRunning
	commitLockPIDRunning = func(pid int) (bool, error) {
		if pid != 12345 {
			t.Fatalf("pid probe = %d, want 12345", pid)
		}
		return false, nil
	}
	t.Cleanup(func() { commitLockPIDRunning = originalProbe })

	if err := removeStaleCommitLock(lockPath, 10*time.Minute); err != nil {
		t.Fatalf("removeStaleCommitLock() error = %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file still exists, stat err = %v", err)
	}
}

func TestRemoveStaleCommitLockKeepsFreshLiveOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("42\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	originalProbe := commitLockPIDRunning
	commitLockPIDRunning = func(pid int) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { commitLockPIDRunning = originalProbe })

	if err := removeStaleCommitLock(lockPath, 10*time.Minute); err != nil {
		t.Fatalf("removeStaleCommitLock() error = %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file should remain for live owner, stat err = %v", err)
	}
}

func TestRemoveStaleCommitLockKeepsFreshMalformedOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	originalProbe := commitLockPIDRunning
	commitLockPIDRunning = func(pid int) (bool, error) {
		t.Fatalf("commitLockPIDRunning should not be called for malformed lock content")
		return false, nil
	}
	t.Cleanup(func() { commitLockPIDRunning = originalProbe })

	if err := removeStaleCommitLock(lockPath, 10*time.Minute); err != nil {
		t.Fatalf("removeStaleCommitLock() error = %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("fresh malformed lock should remain, stat err = %v", err)
	}
}

func TestRemoveStaleCommitLockRemovesStaleMalformedOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}
	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(lock) error = %v", err)
	}

	if err := removeStaleCommitLock(lockPath, time.Minute); err != nil {
		t.Fatalf("removeStaleCommitLock() error = %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale malformed lock should be removed, stat err = %v", err)
	}
}

// TestRemoveStaleCommitLockRemovesAgedLiveOwner pins the deliberate shape of
// the age arm: age alone reclaims even from an owner whose PID probes alive.
// This is the release valve for holders that are alive but not working — a
// SIGSTOPped/frozen process, or a foreign host's dead holder whose recorded
// PID happens to alias a live local process (the case a dead-PID requirement
// would wedge forever). Live WORKING holders never reach this cell: their
// heartbeat keeps the mtime inside the window
// (TestCommitLockHeartbeatProtectsLiveHolderFromAgeReclaim).
func TestRemoveStaleCommitLockRemovesAgedLiveOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("4242\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}
	staleTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatalf("Chtimes(lock) error = %v", err)
	}

	originalProbe := commitLockPIDRunning
	commitLockPIDRunning = func(pid int) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { commitLockPIDRunning = originalProbe })

	if err := removeStaleCommitLock(lockPath, time.Minute); err != nil {
		t.Fatalf("removeStaleCommitLock() error = %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("aged lock should be reclaimed even from a live-probing owner, stat err = %v", err)
	}
}

// TestCommitLockHeartbeatProtectsLiveHolderFromAgeReclaim pins the
// links-snapshots-3dtv.1 acceptance shape on a compressed timeline: a live
// holder that works far longer than the staleness window cannot lose the lock
// to age alone, because its heartbeat keeps the mtime inside the window — and
// the moment the holder stops beating (frozen, or killed mid-hold), a
// contender reclaims promptly. The holder is simulated as a FOREIGN process
// (lock file written directly, heartbeat run against it) because an
// in-process contender parks on processCommitMutex and never reaches the
// stale-file logic.
func TestCommitLockHeartbeatProtectsLiveHolderFromAgeReclaim(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("4242\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	originalProbe := commitLockPIDRunning
	commitLockPIDRunning = func(pid int) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { commitLockPIDRunning = originalProbe })

	// 40x beat-to-staleness margin — wider than production's 10x — because a
	// compressed timeline is exposed to real scheduler freezes: a whole-process
	// stall longer than the staleness window (hypervisor pause, cgroup
	// throttling) starves the heartbeat and the contender's next poll steals
	// legitimately. At 1s a stall must outlast a full second to flake, versus
	// 250ms where CI-plausible pauses reproduced the steal deterministically.
	originalStale := commitLockStaleAfter
	originalBeat := commitLockHeartbeatEvery
	commitLockStaleAfter = time.Second
	commitLockHeartbeatEvery = 25 * time.Millisecond
	t.Cleanup(func() {
		commitLockStaleAfter = originalStale
		commitLockHeartbeatEvery = originalBeat
	})

	stopHeartbeat := startCommitLockHeartbeat(lockPath)

	contendCtx, cancel := context.WithTimeout(context.Background(), 6*commitLockStaleAfter)
	defer cancel()
	release, err := acquireCommitLockAtPath(contendCtx, lockPath)
	if err == nil {
		release()
		stopHeartbeat()
		t.Fatalf("contender acquired a live, heartbeating holder's lock — age reclaim stole from a live holder")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		stopHeartbeat()
		t.Fatalf("contender error = %v, want context.DeadlineExceeded from waiting out a held lock", err)
	}

	content, readErr := os.ReadFile(lockPath)
	if readErr != nil {
		t.Fatalf("lock file should survive the contention window, read err = %v", readErr)
	}
	if string(content) != "4242\n" {
		t.Fatalf("lock content = %q, want the original holder's %q — a steal-and-reacquire cycle rewrote it", content, "4242\n")
	}
	info, statErr := os.Stat(lockPath)
	if statErr != nil {
		t.Fatalf("Stat(lock) error = %v", statErr)
	}
	if age := time.Since(info.ModTime()); age > commitLockStaleAfter {
		t.Fatalf("lock mtime is %v old after contention — the heartbeat was not keeping it fresh", age)
	}

	stopHeartbeat()

	reclaimCtx, cancelReclaim := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReclaim()
	release, err = acquireCommitLockAtPath(reclaimCtx, lockPath)
	if err != nil {
		t.Fatalf("contender should reclaim once the holder stops beating, error = %v", err)
	}
	release()
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("release should remove lock file, stat err = %v", err)
	}
}

// TestAcquireCommitLockStartsHeartbeatAndReleaseStopsIt pins that the
// heartbeat is owned by the acquisition primitive itself — Store mutations
// and external LockCommitPath callers alike hold under it without opting in —
// by backdating the held lock's mtime and watching a real on-disk beat pull
// it back to now, and that release removes the file. The stop-before-remove
// ordering is pinned separately and deterministically by
// TestCommitLockReleaseStopsHeartbeatBeforeRemove.
func TestAcquireCommitLockStartsHeartbeatAndReleaseStopsIt(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")

	originalBeat := commitLockHeartbeatEvery
	commitLockHeartbeatEvery = 25 * time.Millisecond
	t.Cleanup(func() { commitLockHeartbeatEvery = originalBeat })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := acquireCommitLockAtPath(ctx, lockPath)
	if err != nil {
		t.Fatalf("acquireCommitLockAtPath() error = %v", err)
	}

	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lockPath, past, past); err != nil {
		release()
		t.Fatalf("Chtimes(backdate) error = %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		info, statErr := os.Stat(lockPath)
		if statErr != nil {
			release()
			t.Fatalf("Stat(lock) error = %v", statErr)
		}
		if time.Since(info.ModTime()) < time.Minute {
			break
		}
		if time.Now().After(deadline) {
			release()
			t.Fatalf("heartbeat never refreshed the backdated mtime")
		}
		time.Sleep(10 * time.Millisecond)
	}

	release()
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("release should remove lock file, stat err = %v", err)
	}
}

// TestCommitLockReleaseStopsHeartbeatBeforeRemove pins the release ordering
// contract deterministically through the commitLockTouch seam: release joins
// any in-flight beat (it cannot return while one is executing), the lock file
// outlives every beat (a beat never observes the path already removed), and
// no beat runs after release returns. Mutation-tested: dropping the
// stopped-join lets release return during the blocked beat; reordering
// remove-before-stop makes the blocked beat observe a missing file; never
// stopping the goroutine fires a beat after release — each is caught by
// construction, not by timing.
func TestCommitLockReleaseStopsHeartbeatBeforeRemove(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")

	originalBeat := commitLockHeartbeatEvery
	originalTouch := commitLockTouch
	commitLockHeartbeatEvery = 10 * time.Millisecond
	t.Cleanup(func() {
		commitLockHeartbeatEvery = originalBeat
		commitLockTouch = originalTouch
	})

	firstTouchStarted := make(chan struct{})
	firstTouchProceed := make(chan struct{})
	var firstOnce sync.Once
	var afterRelease atomic.Bool
	var violation atomic.Value
	commitLockTouch = func(path string, atime, mtime time.Time) error {
		isFirst := false
		firstOnce.Do(func() { isFirst = true })
		if isFirst {
			close(firstTouchStarted)
			<-firstTouchProceed
		}
		if afterRelease.Load() {
			violation.Store("a beat ran after release returned")
		}
		if _, err := os.Stat(path); err != nil {
			violation.Store("lock file missing during an in-flight beat: " + err.Error())
		}
		return os.Chtimes(path, atime, mtime)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := acquireCommitLockAtPath(ctx, lockPath)
	if err != nil {
		t.Fatalf("acquireCommitLockAtPath() error = %v", err)
	}

	<-firstTouchStarted

	releaseDone := make(chan struct{})
	go func() {
		release()
		close(releaseDone)
	}()

	// The beat is still blocked inside commitLockTouch; a correct release is
	// structurally unable to return yet. Only the dropped-join mutation can
	// close releaseDone here.
	select {
	case <-releaseDone:
		close(firstTouchProceed)
		t.Fatalf("release returned while a beat was in flight — the stop join is gone")
	case <-time.After(100 * time.Millisecond):
	}

	close(firstTouchProceed)
	<-releaseDone
	afterRelease.Store(true)

	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("release should remove lock file, stat err = %v", err)
	}

	// Give a leaked heartbeat several beat intervals to betray itself.
	time.Sleep(5 * commitLockHeartbeatEvery)
	if v := violation.Load(); v != nil {
		t.Fatalf("release ordering violated: %s", v)
	}
}

func TestAcquireCommitLockReclaimsDeadOwner(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), ".links-commit.lock")
	if err := os.WriteFile(lockPath, []byte("99999\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}
	s := &Store{commitLockPath: lockPath}

	originalProbe := commitLockPIDRunning
	commitLockPIDRunning = func(pid int) (bool, error) {
		return false, nil
	}
	t.Cleanup(func() { commitLockPIDRunning = originalProbe })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lockedCtx, release, err := s.acquireCommitLock(ctx)
	if err != nil {
		t.Fatalf("acquireCommitLock() error = %v", err)
	}
	if lockedCtx.Value(commitLockContextKey{}) != true {
		t.Fatalf("acquireCommitLock() did not set commit lock context value")
	}
	release()

	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("release should remove lock file, stat err = %v", err)
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
