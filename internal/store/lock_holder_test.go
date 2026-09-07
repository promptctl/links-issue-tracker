package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promptctl/primitives/filelock"
)

// syncWriter collects notice output written from announceLockWait's reporter
// goroutine while the test reads it, so the race detector sees the guard the
// two sides actually share.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// captureLockNotices routes operator notices into a buffer for the duration of
// one test and shrinks the notice schedule so a wait reports within it. Serial
// by construction: it mutates package state, so no caller may t.Parallel.
func captureLockNotices(t *testing.T, grace, interval time.Duration) *syncWriter {
	t.Helper()
	writer := &syncWriter{}
	prevWriter, prevGrace, prevInterval := lockWaitNoticeWriter, lockWaitNoticeGrace, lockWaitNoticeInterval
	lockWaitNoticeWriter, lockWaitNoticeGrace, lockWaitNoticeInterval = writer, grace, interval
	t.Cleanup(func() {
		lockWaitNoticeWriter, lockWaitNoticeGrace, lockWaitNoticeInterval = prevWriter, prevGrace, prevInterval
	})
	return writer
}

// storageDirOf names the lit storage dir for a test lock — the directory the
// lock file itself sits in, which is where every lit-minted lock lives.
func storageDirOf(lockPath string) string {
	return filepath.Dir(lockPath)
}

// blockHolderDir puts a regular file where the holder directory belongs, so
// every path through recording and reading fails with something other than
// not-exist — the case that must surface rather than read as "no holders".
func blockHolderDir(t *testing.T, lockPath string) {
	t.Helper()
	dir := lockHolderDir(storageDirOf(lockPath), lockPath)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatalf("mkdir holder root: %v", err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
}

// TestContentionErrorNamesTheLiveHolder is this file's headline contract, and
// the one the reported bug turned on: a command that cannot take a lock must
// say WHO is holding it — pid, command, and how long — rather than leaving the
// operator to guess between a wedged holder, a slow query, and a hung fetch.
//
// The holder is a second hold taken in this process: two open file
// descriptions contend through flock exactly as two processes do, so the
// contention under test is the real one and needs no subprocess.
func TestContentionErrorNamesTheLiveHolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	holder, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, true, 1, 0)
	if err != nil {
		t.Fatalf("holder acquireStoreLock() error = %v", err)
	}
	defer func() {
		if err := holder(); err != nil {
			t.Errorf("release holder: %v", err)
		}
	}()

	_, err = acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, true, 1, 0)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("contended acquireStoreLock() error = %v, want ErrWorkspaceBusy", err)
	}
	message := err.Error()
	for _, want := range []string{
		"pid " + strconv.Itoa(os.Getpid()),
		"holding since",
		filepath.Base(os.Args[0]),
	} {
		if !strings.Contains(message, want) {
			t.Errorf("contention error %q does not name the holder's %q", message, want)
		}
	}
}

// TestWaitAnnouncesItselfRepeatedly pins the other half of the report: silence
// is what made a wedged lock indistinguishable from slow work, so a wait that
// outlasts the grace reports itself, names its holder, and KEEPS reporting —
// one line that scrolled away fifteen minutes ago is the same silence.
func TestWaitAnnouncesItselfRepeatedly(t *testing.T) {
	notices := captureLockNotices(t, 10*time.Millisecond, 10*time.Millisecond)
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	holder, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, true, 1, 0)
	if err != nil {
		t.Fatalf("holder acquireStoreLock() error = %v", err)
	}
	defer func() {
		if err := holder(); err != nil {
			t.Errorf("release holder: %v", err)
		}
	}()

	// A budget long enough to outlast several notice intervals; the wait ends
	// by exhausting it, so the reporter is stopped by the acquisition
	// finishing rather than by the test.
	if _, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, true, 30, 10*time.Millisecond); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("contended acquireStoreLock() error = %v, want ErrWorkspaceBusy", err)
	}

	printed := notices.String()
	if got := strings.Count(printed, "still waiting for "+lockPath); got < 2 {
		t.Errorf("notices repeated %d time(s), want at least 2:\n%s", got, printed)
	}
	if !strings.Contains(printed, "pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("notices do not name the holder:\n%s", printed)
	}
}

// TestPromptAcquisitionAnnouncesNothing pins the notice's other edge: an
// acquisition that does not wait must print nothing at all. A channel that
// narrates every uncontended lock trains its reader to ignore it, which costs
// exactly the signal the notice exists to carry.
func TestPromptAcquisitionAnnouncesNothing(t *testing.T) {
	notices := captureLockNotices(t, 10*time.Millisecond, 10*time.Millisecond)
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	release, err := acquireStoreLock(context.Background(), storageDirOf(lockPath), lockPath, true, 1, 0)
	if err != nil {
		t.Fatalf("acquireStoreLock() error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Outlast the grace: a reporter that survived its stop would print here.
	time.Sleep(50 * time.Millisecond)
	if printed := notices.String(); printed != "" {
		t.Errorf("uncontended acquisition printed:\n%s", printed)
	}
}

// TestReleaseRetiresTheHolderRecord pins that a record answers for exactly the
// life of its hold. A record outliving its lock would name a departed holder
// to the next contender — the stale-PID-file failure this package's discipline
// exists to keep unrepresentable.
func TestReleaseRetiresTheHolderRecord(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	release, err := acquireStoreLock(context.Background(), storageDirOf(lockPath), lockPath, true, 1, 0)
	if err != nil {
		t.Fatalf("acquireStoreLock() error = %v", err)
	}
	if holders, problems := readLockHolders(storageDirOf(lockPath), lockPath); len(holders) != 1 || len(problems) != 0 {
		t.Fatalf("while held: holders = %v, problems = %v, want exactly one holder and no problems", holders, problems)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	// The filesystem first: reading retires dead records, so a read here would
	// clean up the very leak this assertion is looking for. An uncontended
	// workspace never reads, so a record the release failed to remove would
	// accumulate on every command, unseen.
	if left := recordFiles(t, lockPath); len(left) != 0 {
		t.Errorf("release left %v behind", left)
	}
	if holders, problems := readLockHolders(storageDirOf(lockPath), lockPath); len(holders) != 0 || len(problems) != 0 {
		t.Errorf("after release: holders = %v, problems = %v, want none of either", holders, problems)
	}
}

// recordFiles lists the holder records currently on disk for one lock, without
// reading them — the read path retires dead records, so it cannot be used to
// ask what the writer left behind.
func recordFiles(t *testing.T, lockPath string) []string {
	t.Helper()
	entries, err := os.ReadDir(lockHolderDir(storageDirOf(lockPath), lockPath))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read holder dir: %v", err)
	}
	var records []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), lockHolderRecordPrefix) {
			records = append(records, entry.Name())
		}
	}
	return records
}

// TestDeadHolderRecordIsNeverReported pins that liveness is decided by
// acquiring and by nothing else. A record left behind by a SIGKILLed holder —
// its content perfectly intact, its hold gone with its process — must never be
// reported as a holder, because naming a dead process as the blocker sends its
// reader hunting a PID that no longer exists.
func TestDeadHolderRecordIsNeverReported(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	dir := lockHolderDir(storageDirOf(lockPath), lockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir holder dir: %v", err)
	}
	payload, err := json.Marshal(lockHolderRecord{PID: 424242, Command: "lit backlog", Since: time.Now().Add(-29 * time.Hour)})
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	orphan := filepath.Join(dir, lockHolderRecordPrefix+"orphan")
	if err := os.WriteFile(orphan, payload, 0o600); err != nil {
		t.Fatalf("write orphan record: %v", err)
	}

	holders, problems := readLockHolders(storageDirOf(lockPath), lockPath)
	if len(holders) != 0 || len(problems) != 0 {
		t.Fatalf("holders = %v, problems = %v, want none of either", holders, problems)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("orphan record survived the read: stat err = %v, want not-exist", err)
	}
	if account := describeLockHolders(storageDirOf(lockPath), lockPath); !strings.Contains(account, "left no record") {
		t.Errorf("account = %q, want it to report an unrecorded holder", account)
	}
}

// TestUnrecordedHolderIsReportedAsSuch pins the honest answer for a holder
// this lit never recorded — a foreign process, or an older binary. The account
// must say the holder is unnamed rather than report "nobody", which reads as a
// free lock and contradicts the kernel that just refused it.
func TestUnrecordedHolderIsReportedAsSuch(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	// Straight through the primitive: no acquireStoreLock, so no record.
	release, acquired, err := filelock.Acquire(context.Background(), lockPath, true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("foreign holder: acquired = %v, err = %v", acquired, err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Errorf("release foreign holder: %v", err)
		}
	}()

	_, err = acquireStoreLock(context.Background(), storageDirOf(lockPath), lockPath, true, 1, 0)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("contended acquireStoreLock() error = %v, want ErrWorkspaceBusy", err)
	}
	if !strings.Contains(err.Error(), "left no record") {
		t.Errorf("contention error %q does not report the holder as unrecorded", err)
	}
}

// TestSharedHoldersAreAllNamed pins that the account is complete, not a
// sample. The reported wedge was a shared holder, and an exclusive acquirer
// blocked behind N readers needs all N — an account naming one of three sends
// its reader to kill one process and wait out two it never heard about.
func TestSharedHoldersAreAllNamed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	for i := 0; i < 3; i++ {
		release, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, false, 1, 0)
		if err != nil {
			t.Fatalf("shared holder %d: %v", i, err)
		}
		defer func() {
			if err := release(); err != nil {
				t.Errorf("release shared holder: %v", err)
			}
		}()
	}

	_, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, true, 1, 0)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("exclusive acquireStoreLock() error = %v, want ErrWorkspaceBusy", err)
	}
	if got := strings.Count(err.Error(), "pid "+strconv.Itoa(os.Getpid())); got != 3 {
		t.Errorf("contention error names %d holders, want 3:\n%s", got, err)
	}
}

// TestHolderCommandIsRenderedNotEchoed pins that another process's argv is
// treated as text to render, never as output to emit: an embedded newline
// would otherwise forge a second "lit:" line in an operator's terminal, and an
// escape sequence would repaint it.
func TestHolderCommandIsRenderedNotEchoed(t *testing.T) {
	t.Parallel()
	rendered := renderHolderCommand("lit backlog\nlit: everything is fine\x1b[2J")
	if strings.ContainsAny(rendered, "\n\x1b") {
		t.Errorf("rendered command %q still carries control characters", rendered)
	}
	// Multibyte, so a byte-wise cut would land mid-rune and show as mojibake.
	long := renderHolderCommand(strings.Repeat("é", holderCommandLimit*2))
	if strings.ContainsRune(long, '�') {
		t.Errorf("rendered command %q was cut mid-rune", long)
	}
	if len([]rune(long)) > holderCommandLimit+1 {
		t.Errorf("rendered command is %d runes, want it bounded at %d plus an ellipsis", len([]rune(long)), holderCommandLimit)
	}
}

// TestUnreadableHolderDirIsReportedNotSwallowed pins that a diagnostic which
// cannot do its job says so. Quietly reporting "no holders" when the records
// were merely unreadable would restore the exact silence this file removes,
// and would do it at the moment the operator is already in trouble.
func TestUnreadableHolderDirIsReportedNotSwallowed(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	// A regular file where the holder directory belongs: ReadDir fails with
	// something other than not-exist, which is the case that must surface.
	blockHolderDir(t, lockPath)
	account := describeLockHolders(storageDirOf(lockPath), lockPath)
	if !strings.Contains(account, "holder records unreadable") {
		t.Errorf("account = %q, want it to report the read failure", account)
	}
}

// TestRecordingFailureLeavesTheLockUsable pins the demotion: a lock whose
// holder record cannot be written is still a lock. Failing the acquisition
// over a diagnostic would break a working workspace to protect a message
// nobody asked for — loud, but never a false failure.
func TestRecordingFailureLeavesTheLockUsable(t *testing.T) {
	notices := captureLockNotices(t, time.Hour, time.Hour)
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	// A regular file where the holder directory belongs makes MkdirAll fail,
	// so recording cannot succeed while the lock itself is untouched.
	blockHolderDir(t, lockPath)

	release, err := acquireStoreLock(context.Background(), storageDirOf(lockPath), lockPath, true, 1, 0)
	if err != nil {
		t.Fatalf("acquireStoreLock() error = %v, want the lock despite an unrecordable holder", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if printed := notices.String(); !strings.Contains(printed, "could not record this process as the holder") {
		t.Errorf("recording failure was not reported:\n%s", printed)
	}
}

// TestHolderRecordSurvivesConcurrentAcquisitions pins that the records stay
// consistent under the concurrency they exist to describe: whatever set of
// holders wins, every account is parseable and no record outlives its hold.
func TestHolderRecordSurvivesConcurrentAcquisitions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, false, 50, time.Millisecond)
			if err != nil {
				return
			}
			describeLockHolders(storageDirOf(lockPath), lockPath)
			if err := release(); err != nil {
				t.Errorf("release: %v", err)
			}
		}()
	}
	wg.Wait()

	if left := recordFiles(t, lockPath); len(left) != 0 {
		t.Errorf("records outlived their holds: %v", left)
	}
	holders, problems := readLockHolders(storageDirOf(lockPath), lockPath)
	if len(holders) != 0 || len(problems) != 0 {
		t.Errorf("after every hold released: holders = %v, problems = %v, want none of either", holders, problems)
	}
}

// TestPublishSurvivesASweepRacingIt pins what publishing under two names buys:
// a lock that is held is named to whoever is waiting on it, however hard the
// waiting is sweeping.
//
// A sweep retires every record it proves unheld, and both halves of that proof
// are hostile to a record still being made. It is decided under the record's
// hold and acted on after that hold is gone, and the probe establishing it
// opens with O_CREATE, so a sweep can manufacture the record it retires. Under
// one name they compound: one sweep unlinks a name mid-publish, another
// recreates it empty, and the publisher fills and publishes a file nothing
// holds — a live holder anonymous for the life of its lock. Publishing by
// rename keeps every sweepable name a finished record and every in-flight name
// out of the sweep's sight.
//
// The sweepers are exactly what announceLockWait runs on a contended lock, one
// per waiting contender, so this is the production pairing rather than a
// contrived one — and one sweeper is not the pairing, because the failure this
// pins needs a second one to recreate what the first retired.
//
// The assertion is the whole contract, not the absence of any one failure. A
// sweep can cost a holder its name several ways — an empty record parsing as
// nothing, a published record nothing holds, a mint retired until the publisher
// gives up — and every one of them ends with an account that cannot say who is
// holding the lock. Whether this pid is in it answers all of them at once.
func TestPublishSurvivesASweepRacingIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	storageDir := storageDirOf(lockPath)

	stop := make(chan struct{})
	var sweeps sync.WaitGroup
	for sweeper := 0; sweeper < 4; sweeper++ {
		sweeps.Add(1)
		go func() {
			defer sweeps.Done()
			for {
				select {
				case <-stop:
					return
				default:
					readLockHolders(storageDir, lockPath)
				}
			}
		}()
	}
	defer func() {
		close(stop)
		sweeps.Wait()
	}()

	for i := 0; i < 300; i++ {
		release, err := acquireStoreLock(ctx, storageDir, lockPath, true, 1, 0)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		// The account this holder's own contenders would read, taken while the
		// hold is live — the only moment the record is supposed to answer for
		// it, and the moment a lost record turns into an unnamed holder.
		account := describeLockHolders(storageDir, lockPath)
		if err := release(); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
		if named := fmt.Sprintf("pid %d ", os.Getpid()); !strings.Contains(account, named) {
			t.Fatalf("acquire %d: the sweep cost a live holder its name, want %q in the account: %s", i, named, account)
		}
	}
}

// TestStrayRecordIsRetiredNotLeaked pins that an empty file under a record's
// name never accumulates. Publishing by rename means no publisher writes one —
// but a reader's own probe does, because filelock opens with O_CREATE and a
// sweep meeting a name its holder has just retired recreates it. The sweep that
// made it is the sweep that collects it, so nothing has to know it was ever a
// special case.
func TestStrayRecordIsRetiredNotLeaked(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	dir := lockHolderDir(storageDirOf(lockPath), lockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir holder dir: %v", err)
	}
	minted, err := os.CreateTemp(dir, lockHolderRecordPrefix+"*")
	if err != nil {
		t.Fatalf("mint record: %v", err)
	}
	if err := minted.Close(); err != nil {
		t.Fatalf("close minted record: %v", err)
	}

	holders, problems := readLockHolders(storageDirOf(lockPath), lockPath)
	if len(holders) != 0 || len(problems) != 0 {
		t.Errorf("holders = %v, problems = %v, want an empty record to report neither", holders, problems)
	}
	if left := recordFiles(t, lockPath); len(left) != 0 {
		t.Errorf("an empty record survived the read: %v", left)
	}
}

// TestSweepLeavesAMintAlone pins the cost side of publishing under two names,
// so it stays a decision rather than a discovery. A mint carries no record
// prefix, so no sweep opens it, retires it, or counts it against the account —
// which is exactly what keeps a sweep from destroying one mid-flight, and
// exactly why a publisher killed before its rename leaves a file behind that
// nothing collects. An empty file naming nobody is the cheap end of that trade.
func TestSweepLeavesAMintAlone(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	dir := lockHolderDir(storageDirOf(lockPath), lockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir holder dir: %v", err)
	}
	minting, err := os.CreateTemp(dir, lockHolderMintingPrefix+"*")
	if err != nil {
		t.Fatalf("mint record: %v", err)
	}
	if err := minting.Close(); err != nil {
		t.Fatalf("close mint: %v", err)
	}

	holders, problems := readLockHolders(storageDirOf(lockPath), lockPath)
	if len(holders) != 0 || len(problems) != 0 {
		t.Errorf("holders = %v, problems = %v, want a mint to report neither", holders, problems)
	}
	if _, err := os.Stat(minting.Name()); err != nil {
		t.Errorf("stat mint after a sweep: %v, want a sweep to leave it untouched", err)
	}
}

// TestFillingARetiredRecordFailsRatherThanRecreatingIt pins the O_CREATE-less
// open. A record retired in the instant before its hold landed must not be
// written back into existence: the file would carry this holder's identity
// with nothing holding it, and the next reader would find it ownerless and
// retire it — costing the account a holder that is very much alive, silently.
func TestFillingARetiredRecordFailsRatherThanRecreatingIt(t *testing.T) {
	t.Parallel()
	retired := filepath.Join(t.TempDir(), lockHolderRecordPrefix+"gone")

	if err := fillHeldRecord(retired, []byte(`{"pid":1}`)); err == nil {
		t.Fatal("fillHeldRecord() on a retired record returned no error")
	}
	if _, err := os.Stat(retired); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("fillHeldRecord() recreated the retired record: stat err = %v, want not-exist", err)
	}
}

// TestUnnamedHolderDoesNotGuessPastAFailedRead pins that the account stops
// asserting "left no record" when it could not read the records at all. The
// two facts are different — nothing recorded itself, versus this call could
// not see what did — and printing the first beside "holder records unreadable"
// sends an operator hunting a foreign process over a local read failure.
func TestUnnamedHolderDoesNotGuessPastAFailedRead(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	blockHolderDir(t, lockPath)

	account := describeLockHolders(storageDirOf(lockPath), lockPath)
	if !strings.Contains(account, "holder records unreadable") {
		t.Fatalf("account = %q, want it to report the read failure", account)
	}
	if strings.Contains(account, "left no record") {
		t.Errorf("account = %q, want no claim about what the holder recorded when the records could not be read", account)
	}
}

// TestContentionAccountReachesTheWrappers pins that the holder account is not
// a detail of acquireStoreLock but reaches the operator through the messages
// the wrappers actually print — the reason both halves live at the single
// boundary rather than being re-added at each of the five call sites.
func TestContentionAccountReachesTheWrappers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	holder, err := acquireStoreLock(ctx, workspaceStorageDir(doltRoot), WorkspaceLockPath(doltRoot), true, 1, 0)
	if err != nil {
		t.Fatalf("holder acquireStoreLock() error = %v", err)
	}
	defer func() {
		if err := holder(); err != nil {
			t.Errorf("release holder: %v", err)
		}
	}()

	_, err = LockWorkspaceExclusive(ctx, doltRoot)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("LockWorkspaceExclusive() error = %v, want ErrWorkspaceBusy", err)
	}
	if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.Contains(err.Error(), want) {
		t.Errorf("LockWorkspaceExclusive() error %q does not carry %q", err, want)
	}
}

// TestBeaconContentionNamesTheSquatter pins the one wrapper that built its own
// message rather than carrying the account out. A foreign process holding the
// beacon past every probe window is the case where naming it matters most, and
// it was the only path where the answer was dropped. The sentinel stays
// un-propagated — that classification is deliberate — so the account has to
// travel as text, and both halves are pinned here together.
func TestBeaconContentionNamesTheSquatter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	squatter, err := acquireStoreLock(ctx, workspaceStorageDir(doltRoot), MirrorBeaconLockPath(doltRoot), true, 1, 0)
	if err != nil {
		t.Fatalf("squatter acquireStoreLock() error = %v", err)
	}
	defer func() {
		if err := squatter(); err != nil {
			t.Errorf("release squatter: %v", err)
		}
	}()

	release, err := HoldMirrorBeacon(ctx, doltRoot)
	if err == nil {
		t.Fatalf("HoldMirrorBeacon() succeeded against an exclusive squatter; release = %v", release())
	}
	if errors.Is(err, ErrWorkspaceBusy) {
		t.Errorf("HoldMirrorBeacon() error = %v, want the busy sentinel deliberately not propagated", err)
	}
	if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.Contains(err.Error(), want) {
		t.Errorf("HoldMirrorBeacon() error %q does not name the squatter (%s)", err, want)
	}
}

// TestLockRecordsStayOutOfDoltsTree pins where records may be written. Dolt's
// journal LOCK is the one lock lit takes inside a directory it does not own,
// and records minted beside it would put lit state in Dolt's tree — and copy
// it into every `lit snapshots new` artifact, since the snapshot walks exactly
// that directory. The record still has to exist; it just lives where lit's
// things live.
func TestLockRecordsStayOutOfDoltsTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := migratedDoltDir(t)
	before := treeUnder(t, doltRoot)

	release, err := LockDoltJournalExclusive(ctx, doltRoot)
	if err != nil {
		t.Fatalf("LockDoltJournalExclusive() error = %v", err)
	}
	if after := treeUnder(t, doltRoot); !slices.Equal(before, after) {
		t.Errorf("holding the journal lock changed Dolt's tree:\nbefore %v\nafter  %v", before, after)
	}
	holders, problems := readLockHolders(workspaceStorageDir(doltRoot), DoltJournalLockPath(doltRoot))
	if len(holders) != 1 || len(problems) != 0 {
		t.Errorf("holders = %v, problems = %v, want the one record, kept outside Dolt's tree", holders, problems)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// treeUnder lists every path beneath root, so a test can assert that an
// operation added nothing to a directory it does not own.
func treeUnder(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	slices.Sort(paths)
	return paths
}
