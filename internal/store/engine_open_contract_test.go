package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/primitives/filelock"
)

// journalLockPath is where dolt's journaling store takes its exclusive
// manifest lock for lit's database: <doltRoot>/links/.dolt/noms/LOCK. Holding
// it with an independent fd is exactly what a foreign process's still-open
// engine looks like to a new open — flock conflicts are per open file
// description, so an in-test holder is indistinguishable from another process.
func journalLockPath(doltRoot string) string {
	return filepath.Join(doltRoot, doltDatabaseName, ".dolt", "noms", "LOCK")
}

// TestOpenFailsLoudWhenForeignEngineHoldsJournalLock pins the write-open
// contract at the heart of links-sync-pgct.11: when another process's engine
// holds the journal manifest lock past lit's own engine-flock handoff (the
// teardown straggler window), a write-capable open must fail loudly and
// boundedly — never silently hand back a read-only store whose every later
// write dies as "cannot update manifest: database is read only" with no
// in-process cure. The deterministic foreign holder makes the collision
// certain instead of timing-dependent. [LAW:no-silent-failure]
// [LAW:no-ambient-temporal-coupling]
func TestOpenFailsLoudWhenForeignEngineHoldsJournalLock(t *testing.T) {
	// serial: no t.Parallel — rewrites the package-level
	// coResidentHolderWait budget and asserts a wall-clock bound on the
	// failure.
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	releaseLock, acquired, err := filelock.Acquire(ctx, journalLockPath(doltRoot), true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("take journal lock as foreign holder: acquired=%v err=%v", acquired, err)
	}
	defer func() { _ = releaseLock() }()

	prevBudget := coResidentHolderWait
	coResidentHolderWait = 700 * time.Millisecond
	t.Cleanup(func() { coResidentHolderWait = prevBudget })

	start := time.Now()
	opened, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	elapsed := time.Since(start)
	if err == nil {
		_ = opened.Close()
		t.Fatalf("Open() succeeded against a foreign journal-lock holder; want a loud bounded failure")
	}
	if !errors.Is(err, nbs.ErrDatabaseLocked) {
		t.Fatalf("Open() error = %v; want the nbs.ErrDatabaseLocked contention classification to survive the chain", err)
	}
	// Bounded: the shrunken budget (plus dolt's own per-attempt lock waits)
	// must not balloon toward the production coResidentHolderWait, whose
	// value this comment deliberately does not restate — it is derived, and a
	// figure copied here is one more thing to remember into agreement.
	if elapsed > 10*time.Second {
		t.Fatalf("Open() took %s to fail against a held journal lock; the retry budget is not bounding the wait", elapsed)
	}
}

// TestOpenRecoversOnceForeignJournalHolderReleases pins the recovery half of
// the same contract: the bounded open-retry absorbs a holder that releases
// mid-wait, so the caller sees a working writable store, not an error and not
// a read-only one. The release happens while Open is already retrying; the
// retry loop — not test timing — owns the reconciliation.
func TestOpenRecoversOnceForeignJournalHolderReleases(t *testing.T) {
	// serial: no t.Parallel — rewrites the package-level
	// coResidentHolderWait budget, which would govern every concurrently
	// running Open.
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	releaseLock, acquired, err := filelock.Acquire(ctx, journalLockPath(doltRoot), true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("take journal lock as foreign holder: acquired=%v err=%v", acquired, err)
	}
	release := time.AfterFunc(300*time.Millisecond, func() { _ = releaseLock() })
	defer release.Stop()

	reopened, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() after holder release error = %v; want the open retry to absorb the handoff", err)
	}
	defer reopened.Close()
	// The store must be genuinely writable — the whole point of failing fast
	// instead of accepting dolt's silent read-only fallback.
	epic, err := reopened.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "post-handoff epic", Topic: "sync", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) on the recovered store error = %v", err)
	}
	created, err := reopened.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "post-handoff write works", Topic: "sync", ParentID: epic.ID})
	if err != nil {
		t.Fatalf("CreateIssue() on the recovered store error = %v", err)
	}
	if created.ID == "" {
		t.Fatalf("CreateIssue() returned an empty id")
	}
}

// TestOpenSyncContentionCarriesWorkspaceBusy pins the mirror-facing half of
// the write-open contention contract: when OpenSync times out against a
// foreign journal-lock holder, the error must carry ErrWorkspaceBusy — the
// uniform contention sentinel pushOutcomeOf maps to the non-failed
// workspace_busy outcome — alongside the ErrDatabaseLocked classification.
// Without it a mirror blocked behind a healthy long-running writer records a
// push FAILURE and pages the owner over ordinary serialization.
func TestOpenSyncContentionCarriesWorkspaceBusy(t *testing.T) {
	// serial: no t.Parallel — rewrites the package-level
	// coResidentHolderWait budget, which would govern every concurrently
	// running Open.
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	releaseLock, acquired, err := filelock.Acquire(ctx, journalLockPath(doltRoot), true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("take journal lock as foreign holder: acquired=%v err=%v", acquired, err)
	}
	defer func() { _ = releaseLock() }()

	prevBudget := coResidentHolderWait
	coResidentHolderWait = 700 * time.Millisecond
	t.Cleanup(func() { coResidentHolderWait = prevBudget })

	opened, err := OpenSync(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err == nil {
		_ = opened.Close()
		t.Fatalf("OpenSync() succeeded against a foreign journal-lock holder; want bounded contention")
	}
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("OpenSync() error = %v; want the ErrWorkspaceBusy contention sentinel in the chain", err)
	}
	if !errors.Is(err, nbs.ErrDatabaseLocked) {
		t.Fatalf("OpenSync() error = %v; want the nbs.ErrDatabaseLocked classification preserved alongside the sentinel", err)
	}
}

// TestOpenForReadToleratesForeignJournalHolder pins the read-open contract the
// write-open contention handling must NOT disturb: a read open beside a live
// foreign writer serves reads — reading a store someone else is writing is
// exactly what a read open is for. A read engine never tries the journal lock
// (TestAReadOpenWritesNothing), so the holder is never in its way.
func TestOpenForReadToleratesForeignJournalHolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	epic, err := s.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "visible epic", Topic: "sync", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := s.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "visible to readers", Topic: "sync", ParentID: epic.ID}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	releaseLock, acquired, err := filelock.Acquire(ctx, journalLockPath(doltRoot), true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("take journal lock as foreign holder: acquired=%v err=%v", acquired, err)
	}
	defer func() { _ = releaseLock() }()

	reader, err := OpenForRead(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() beside a journal-lock holder error = %v; want the read-only engine to serve it", err)
	}
	defer reader.Close()
	count, err := reader.LocalIssueCount(ctx)
	if err != nil {
		t.Fatalf("LocalIssueCount() on the reader error = %v", err)
	}
	if count != 2 {
		t.Fatalf("LocalIssueCount() = %d, want 2 (the epic and the ticket inside it)", count)
	}
}

// fileStamp is one entry's bytes and modification time (a directory's bytes
// are empty): two equal stamps mean nothing wrote the entry in between,
// including a write of the bytes it held.
type fileStamp struct {
	data  []byte
	mtime time.Time
}

// stampTree records every regular file and directory under root.
func stampTree(t *testing.T, root string) map[string]fileStamp {
	t.Helper()
	stamps := map[string]fileStamp{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !(d.Type().IsRegular() || d.IsDir()) {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var data []byte
		if !d.IsDir() {
			if data, err = os.ReadFile(path); err != nil {
				return err
			}
		}
		stamps[path] = fileStamp{data: data, mtime: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatalf("stamp %s: %v", root, err)
	}
	return stamps
}

// TestAReadOpenWritesNothing pins links-scale-dy46.boy: a read open, a read
// and a close leave every file and directory under the Dolt root as they
// found it — the
// store's manifest, its journal and the journal's index included, by mtime as
// well as by bytes. A read engine opens Dolt read-only without trying the
// journal lock, so it neither repairs the journal at open nor flushes at
// close. The lock is free here, the case in which a read engine used to take
// it and write.
func TestAReadOpenWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	epic, err := s.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "read epic", Topic: "sync", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if _, err := s.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "read payload", Topic: "sync", ParentID: epic.ID}); err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Every entry is dated an hour back, so a write of unchanged bytes moves
	// its mtime off that date on any filesystem, whatever its granularity.
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := filepath.WalkDir(doltRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, past, past)
	}); err != nil {
		t.Fatalf("backdate %s: %v", doltRoot, err)
	}
	before := stampTree(t, doltRoot)
	reader, err := OpenForRead(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v", err)
	}
	if count, err := reader.LocalIssueCount(ctx); err != nil || count != 2 {
		_ = reader.Close()
		t.Fatalf("LocalIssueCount() = %d, %v; want 2, nil", count, err)
	}
	if _, err := reader.ListIssues(ctx, storage.ListIssuesFilter{}); err != nil {
		_ = reader.Close()
		t.Fatalf("ListIssues() error = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("reader Close() error = %v", err)
	}
	after := stampTree(t, doltRoot)

	for path, b := range before {
		a, ok := after[path]
		switch {
		case !ok:
			t.Errorf("%s is gone after a read", path)
		case string(a.data) != string(b.data):
			t.Errorf("%s bytes changed under a read (%d -> %d bytes)", path, len(b.data), len(a.data))
		case !a.mtime.Equal(b.mtime):
			t.Errorf("%s was written by a read (mtime %s -> %s)", path, b.mtime.Format(time.RFC3339Nano), a.mtime.Format(time.RFC3339Nano))
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s was created by a read", path)
		}
	}
}

// TestAWriteOpensBesideAnOpenReader pins that a reader keeps no writer out: a
// read engine holds no journal lock, so a write open beside a live reader opens
// at once instead of waiting the reader out, and the reader goes on answering
// from the root it opened on (doc.go, ONE ROOT PER ENGINE).
func TestAWriteOpensBesideAnOpenReader(t *testing.T) {
	// serial: no t.Parallel — rewrites the package-level coResidentHolderWait,
	// so a writer that did wait on the reader fails within the test's time.
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	epic, err := s.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "beside epic", Topic: "sync", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	prevBudget := coResidentHolderWait
	coResidentHolderWait = 300 * time.Millisecond
	t.Cleanup(func() { coResidentHolderWait = prevBudget })

	reader, err := OpenForRead(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v", err)
	}
	defer reader.Close()

	writer, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() beside an open reader error = %v; a reader must hold no journal lock", err)
	}
	if _, err := writer.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "written beside a reader", Topic: "sync", ParentID: epic.ID}); err != nil {
		_ = writer.Close()
		t.Fatalf("CreateIssue() beside an open reader error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer Close() error = %v", err)
	}

	if count, err := reader.LocalIssueCount(ctx); err != nil || count != 1 {
		t.Fatalf("reader LocalIssueCount() after the write = %d, %v; want 1 (the root it opened on), nil", count, err)
	}
}

// TestWriteOpenNamesTheJournalHolder is the ticket's contract in-process
// (links-scale-om3r.zhq): with the store held the way `lit snapshots new`
// holds it — Dolt's LOCK taken exclusively through LockDoltJournalExclusive,
// which records the holder — a write open stops trying after
// coResidentHolderWait and fails naming the holder's pid, command and age,
// carrying ErrWorkspaceBusy so the CLI exits with the workspace-busy code.
func TestWriteOpenNamesTheJournalHolder(t *testing.T) {
	// serial: no t.Parallel — rewrites the package-level coResidentHolderWait.
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	releaseJournal, err := LockDoltJournalExclusive(WithHolderCommand(ctx, "lit backlog"), doltRoot)
	if err != nil {
		t.Fatalf("LockDoltJournalExclusive() error = %v", err)
	}
	defer func() { _ = releaseJournal() }()

	prevWait := coResidentHolderWait
	coResidentHolderWait = 300 * time.Millisecond
	t.Cleanup(func() { coResidentHolderWait = prevWait })

	start := time.Now()
	opened, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	elapsed := time.Since(start)
	if err == nil {
		_ = opened.Close()
		t.Fatalf("Open() succeeded against a held journal lock; want a bounded, holder-naming failure")
	}
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("Open() error = %v; want ErrWorkspaceBusy, the sentinel the CLI maps to the workspace-busy exit", err)
	}
	message := err.Error()
	for _, want := range []string{
		"pid " + strconv.Itoa(os.Getpid()) + " (lit backlog)",
		"holding since",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("contention error %q does not name the holder's %q", message, want)
		}
	}
	if elapsed < coResidentHolderWait {
		t.Fatalf("Open() failed after %s, before the %s wait", elapsed, coResidentHolderWait)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Open() took %s to fail against a held journal lock; the contract is a refusal within seconds", elapsed)
	}
}

// TestStoreOutlivesTheContextItWasOpenedUnder pins that a store's life is its
// own, not its opener's: once the context Open ran under is cancelled, a
// mutation on a fresh context still begins, stages and commits. The
// connection is opened inside Open, so a connection that kept Open's context
// would fail every later commit with "context canceled".
func TestStoreOutlivesTheContextItWasOpenedUnder(t *testing.T) {
	t.Parallel()
	openCtx, cancel := context.WithCancel(context.Background())
	st, err := Open(openCtx, testEnv(), migratedDoltDir(t), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()
	cancel()

	ctx := context.Background()
	created, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "after the opener gave up", Topic: "ctx", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue() after cancelling the open context error = %v", err)
	}
	if _, err := st.GetIssue(ctx, created.ID); err != nil {
		t.Fatalf("GetIssue(%s) error = %v; the committed issue is not readable", created.ID, err)
	}
}

// TestAReaderOutlivesAFullCompactionBesideIt pins what a read engine that
// keeps no writer out has to survive: a full DOLT_GC from a write engine while
// the reader is open, which drops the journal the reader bootstrapped from
// and the table files its root was read from. The reader keeps answering from
// the root it opened on, through the files it already holds open.
func TestAReaderOutlivesAFullCompactionBesideIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	epic, err := s.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "gc epic", Topic: "sync", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(epic) error = %v", err)
	}
	for i := 0; i < 20; i++ {
		if _, err := s.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "gc payload " + strconv.Itoa(i), Topic: "sync", ParentID: epic.ID}); err != nil {
			t.Fatalf("CreateIssue() error = %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reader, err := OpenForRead(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v", err)
	}
	defer reader.Close()

	writer, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() beside the reader error = %v", err)
	}
	if _, err := writer.SyncCompact(ctx, GCFull); err != nil {
		_ = writer.Close()
		t.Fatalf("SyncCompact(full) beside an open reader error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer Close() error = %v", err)
	}

	issues, err := reader.ListIssues(ctx, storage.ListIssuesFilter{})
	if err != nil {
		t.Fatalf("reader ListIssues() after a full compaction beside it error = %v", err)
	}
	if len(issues) != 21 {
		t.Fatalf("reader ListIssues() after a full compaction = %d issues, want 21", len(issues))
	}
}
