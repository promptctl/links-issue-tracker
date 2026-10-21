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

// syncWriter collects notice output written by every goroutine contending in
// a test — each wait prints from its own — so the race detector sees the
// guard the writers actually share.
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

// TestContentionErrorNamesTheLiveHolder is this file's headline contract: a
// command that cannot take a lock must say WHO is holding it — pid, command,
// and how long — rather than leaving the operator to guess between a wedged
// holder, a slow query, and a hung fetch.
//
// The holder is a second hold taken in this process: two open file
// descriptions contend through flock exactly as two processes do, so the
// contention under test is the real one and needs no subprocess.
func TestContentionErrorNamesTheLiveHolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	holder, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, true, 0)
	if err != nil {
		t.Fatalf("holder acquireStoreLock() error = %v", err)
	}
	defer func() {
		if err := holder(); err != nil {
			t.Errorf("release holder: %v", err)
		}
	}()

	_, err = acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, true, 0)
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

// TestWaitAnnouncesItselfRepeatedly pins the other half: silence is what makes
// a wedged lock indistinguishable from slow work, so a wait that outlasts the
// grace reports itself, names its holder, and KEEPS reporting — one line that
// scrolled away fifteen minutes ago is the same silence.
//
// The figure each notice reports is the whole wait, not the stand-still
// clock that arrivals restart: a contender behind a queue of short holders
// that said "after 2s" thirty seconds in would hide exactly the wait the
// notice exists to show.
//
// The contender's clock steps one second per poll, so the schedule is read
// off the instants the wait is handed rather than off how fast the machine
// runs. Shared holders hand the lock along at the first three polls, so the
// last arrives at 3s and the 10s wait refuses at 13s; the notice is due at
// the 3s grace and then every 2s interval, which is 3s, 5s, 7s, 9s and 11s.
// The 13s poll is due a notice too and prints none: it is the refusal, and a
// wait that has given up is not still waiting.
func TestWaitAnnouncesItselfRepeatedly(t *testing.T) {
	notices := captureLockNotices(t, 3*time.Second, 2*time.Second)
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	holder, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, false, 0)
	if err != nil {
		t.Fatalf("holder acquireStoreLock() error = %v", err)
	}
	defer func() {
		if err := holder(); err != nil {
			t.Errorf("release holder: %v", err)
		}
	}()

	clock := newStepClock(t, time.Second, func(read int) {
		if read == 0 || read > 3 {
			return
		}
		next, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, false, 0)
		if err != nil {
			t.Fatalf("holder %d: %v", read+1, err)
		}
		previous := holder
		holder = next
		if err := previous(); err != nil {
			t.Fatalf("release holder %d: %v", read, err)
		}
	})
	if _, err := acquireStoreLockOn(ctx, clock.now, storageDirOf(lockPath), lockPath, true, 10*time.Second); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("contended acquireStoreLockOn() error = %v, want ErrWorkspaceBusy", err)
	}

	printed := notices.String()
	var after []string
	for _, line := range strings.Split(strings.TrimSuffix(printed, "\n"), "\n") {
		rest, isNotice := strings.CutPrefix(line, "lit: still waiting for "+lockPath+" after ")
		waited, _, hasAccount := strings.Cut(rest, "; ")
		if !isNotice || !hasAccount {
			t.Fatalf("notice %q is not a wait notice for %s", line, lockPath)
		}
		after = append(after, waited)
	}
	if want := []string{"3s", "5s", "7s", "9s", "11s"}; !slices.Equal(after, want) {
		t.Errorf("notices reported waits of %v, want %v:\n%s", after, want, printed)
	}
	if !strings.Contains(printed, "pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("notices do not name the holder:\n%s", printed)
	}
}

// TestPromptAcquisitionAnnouncesNothing pins the notice's other edge: an
// acquisition that is never refused prints nothing at all, however long it
// took. A channel that narrates every uncontended lock trains its reader to
// ignore it, which costs exactly the signal the notice exists to carry — and
// for Dolt's LOCK, whose foreign holders leave no record, the narration names
// "a process that left no record", a holder that does not exist.
//
// The grace and interval are zero, so a notice is due the instant the wait
// starts: the only thing standing between this acquisition and a printed line
// is that nothing refused it. No clock decides the verdict.
func TestPromptAcquisitionAnnouncesNothing(t *testing.T) {
	notices := captureLockNotices(t, 0, 0)
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	release, err := acquireStoreLock(context.Background(), storageDirOf(lockPath), lockPath, true, 0)
	if err != nil {
		t.Fatalf("acquireStoreLock() error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
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

	release, err := acquireStoreLock(context.Background(), storageDirOf(lockPath), lockPath, true, 0)
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

// A holder record names the command of the run that took the lock, which
// its ctx carries, so two runs in one process are told apart. A lock taken
// outside any run names none.
func TestHolderRecordNamesTheRunsCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"named run", WithHolderCommand(context.Background(), "lit backlog"), "lit backlog"},
		{"no run", context.Background(), ""},
	} {
		lockPath := filepath.Join(t.TempDir(), "test.lock")
		release, err := acquireStoreLock(tc.ctx, storageDirOf(lockPath), lockPath, true, 0)
		if err != nil {
			t.Fatalf("%s: acquireStoreLock() error = %v", tc.name, err)
		}
		holders, problems := readLockHolders(storageDirOf(lockPath), lockPath)
		if err := release(); err != nil {
			t.Fatalf("%s: release: %v", tc.name, err)
		}
		if len(holders) != 1 || len(problems) != 0 || holders[0].Command != tc.want {
			t.Errorf("%s: holders = %+v, problems = %v; want one holder with command %q", tc.name, holders, problems, tc.want)
		}
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

	_, err = acquireStoreLock(context.Background(), storageDirOf(lockPath), lockPath, true, 0)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("contended acquireStoreLock() error = %v, want ErrWorkspaceBusy", err)
	}
	if !strings.Contains(err.Error(), "left no record") {
		t.Errorf("contention error %q does not report the holder as unrecorded", err)
	}
}

// TestSharedHoldersAreAllNamed pins that the account is complete, not a
// sample. An exclusive acquirer blocked behind N readers needs all N — an
// account naming one of three sends its reader to kill one process and wait
// out two it never heard about.
func TestSharedHoldersAreAllNamed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	for i := 0; i < 3; i++ {
		release, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, false, 0)
		if err != nil {
			t.Fatalf("shared holder %d: %v", i, err)
		}
		defer func() {
			if err := release(); err != nil {
				t.Errorf("release shared holder: %v", err)
			}
		}()
	}

	_, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, true, 0)
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

	release, err := acquireStoreLock(context.Background(), storageDirOf(lockPath), lockPath, true, 0)
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
			release, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, false, 50*time.Millisecond)
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
// The sweepers are what every contender on a held lock runs — its wait notice
// and its refusal each render the holder account, which sweeps — one per
// waiting contender, so this is the production pairing rather than a
// contrived one, run back to back where production sweeps once a notice
// interval. One sweeper is not the pairing, because the failure this pins
// needs a second one to recreate what the first retired.
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
		release, err := acquireStoreLock(ctx, storageDir, lockPath, true, 0)
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
// name never accumulates. Publishing by link means no publisher writes one —
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

// TestSweepLeavesAPrivateNameAlone pins the cost side of living under two
// names, so it stays a decision rather than a discovery. A private name
// carries no record prefix, so no sweep opens it, retires it, or counts it
// against the account — which is exactly what keeps a sweep from destroying a
// record mid-flight, and exactly why a publisher killed before it links leaves
// a file behind that nothing collects. A file nothing ever opens is the cheap
// end of that trade — it is staged filled here because the fill lands before
// the link, so most of that window leaves a complete record, not an empty one.
func TestSweepLeavesAPrivateNameAlone(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	dir := lockHolderDir(storageDirOf(lockPath), lockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir holder dir: %v", err)
	}
	payload, err := json.Marshal(lockHolderRecord{PID: 424243, Command: "lit ls", Since: time.Now()})
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	private := filepath.Join(dir, lockHolderPrivatePrefix+"424243-1")
	if err := os.WriteFile(private, payload, 0o600); err != nil {
		t.Fatalf("write private record: %v", err)
	}

	holders, problems := readLockHolders(storageDirOf(lockPath), lockPath)
	if len(holders) != 0 || len(problems) != 0 {
		t.Errorf("holders = %v, problems = %v, want a private name to report neither", holders, problems)
	}
	if _, err := os.Stat(private); err != nil {
		t.Errorf("stat private record after a sweep: %v, want a sweep to leave it untouched", err)
	}
}

// TestSweepRetiresBothNamesOfADeadRecord pins that a record's two names die
// together. A record lives under a swept name and a private one, and only the
// swept name is ever proved dead — so a sweep that retired that one alone
// would strand the private sibling with nothing left in the system that could
// ever collect it, turning every killed holder into permanent debris. One
// proof, one inode, both names.
func TestSweepRetiresBothNamesOfADeadRecord(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	storageDir := storageDirOf(lockPath)

	// The two linked names a SIGKILLed holder leaves behind: content intact,
	// hold gone with the process.
	dir := lockHolderDir(storageDir, lockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir holder dir: %v", err)
	}
	payload, err := json.Marshal(lockHolderRecord{PID: 424242, Command: "lit backlog", Since: time.Now()})
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	record := filepath.Join(dir, lockHolderRecordPrefix+"424242-1")
	if err := os.WriteFile(record, payload, 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
	if err := os.Link(record, privateNameFor(record)); err != nil {
		t.Fatalf("link private name: %v", err)
	}

	if _, problems := readLockHolders(storageDir, lockPath); len(problems) != 0 {
		t.Errorf("problems = %v, want a dead record to be retired quietly", problems)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read holder dir: %v", err)
	}
	if len(left) != 0 {
		names := make([]string, 0, len(left))
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Errorf("names surviving the sweep = %v, want both retired together", names)
	}
}

// strandDeadRecord leaves the two linked names a holder that never ran its
// release leaves behind — a SIGKILLed one, or a mirror-beacon claimant that
// leaves its hold to process exit: content intact, hold gone.
func strandDeadRecord(t *testing.T, dir, suffix string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir holder dir: %v", err)
	}
	payload, err := json.Marshal(lockHolderRecord{PID: 424242, Command: "lit comment add", Since: time.Now()})
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	record := filepath.Join(dir, lockHolderRecordPrefix+suffix)
	if err := os.WriteFile(record, payload, 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
	if err := os.Link(record, privateNameFor(record)); err != nil {
		t.Fatalf("link private name: %v", err)
	}
}

// holderDirEntries lists every name under a lock's holder directory, private
// ones included, without probing any of them.
func holderDirEntries(t *testing.T, lockPath string) []string {
	t.Helper()
	entries, err := os.ReadDir(lockHolderDir(storageDirOf(lockPath), lockPath))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read holder dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestEveryAcquisitionRetiresTheRecordsOfDepartedHolders pins that a record
// does not outlive its holder by more than one acquisition. A holder that
// never runs its release leaves both names behind, and an uncontended lock has
// no contender to read them, so before every acquisition swept, the mirror
// beacon's directory grew by a record per spawning command, without bound.
// The live holder is the other side of the contract: a sweep that took it too
// would cost a holding process its name.
func TestEveryAcquisitionRetiresTheRecordsOfDepartedHolders(t *testing.T) {
	notices := captureLockNotices(t, time.Hour, time.Hour)
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	storageDir := storageDirOf(lockPath)
	dir := lockHolderDir(storageDir, lockPath)

	live, err := acquireStoreLock(ctx, storageDir, lockPath, false, 0)
	if err != nil {
		t.Fatalf("live holder: %v", err)
	}
	for i := range 20 {
		strandDeadRecord(t, dir, fmt.Sprintf("424242-%d", i))
	}

	for round := range 5 {
		release, err := acquireStoreLock(ctx, storageDir, lockPath, false, 0)
		if err != nil {
			t.Fatalf("round %d: acquire: %v", round, err)
		}
		// Two holders, two names each: the live one and this acquisition.
		if left := holderDirEntries(t, lockPath); len(left) != 4 {
			t.Fatalf("round %d: holder dir = %v, want the two live holders' four names and nothing else", round, left)
		}
		if err := release(); err != nil {
			t.Fatalf("round %d: release: %v", round, err)
		}
		strandDeadRecord(t, dir, fmt.Sprintf("424243-%d", round))
	}
	if holders, _ := readLockHolders(storageDir, lockPath); len(holders) != 1 {
		t.Errorf("holders = %v, want the live holder still named after every sweep", holders)
	}
	if err := live(); err != nil {
		t.Fatalf("release live holder: %v", err)
	}
	if printed := notices.String(); printed != "" {
		t.Errorf("sweeping printed %q, want nothing", printed)
	}
}

// TestAReusedPidPublishesPastItsPredecessorsNames pins that a process whose
// pid the kernel handed out before can still name itself. A predecessor with
// that pid may have stranded names nothing collects — a private name from a
// kill before its link — and a process drawing the same names would have its
// publish refused: the reported "create holder record: ... file exists".
//
// The predecessor is drawn through lockHolderRecordNamesFor itself, as a
// process with this pid but its own nonce and a sequence starting over, so
// the test follows whatever the names are made of.
func TestAReusedPidPublishesPastItsPredecessorsNames(t *testing.T) {
	notices := captureLockNotices(t, time.Hour, time.Hour)
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	storageDir := storageDirOf(lockPath)
	dir := lockHolderDir(storageDir, lockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir holder dir: %v", err)
	}

	for seq := range lockHolderRecordSeq.Load() + 50 {
		private, _ := lockHolderRecordNamesFor(dir, os.Getpid(), "0dead0ff", seq+1)
		if err := os.WriteFile(private, nil, 0o600); err != nil {
			t.Fatalf("strand predecessor's private name: %v", err)
		}
	}

	release, err := acquireStoreLock(context.Background(), storageDir, lockPath, true, 0)
	if err != nil {
		t.Fatalf("acquireStoreLock() error = %v", err)
	}
	account := describeLockHolders(storageDir, lockPath)
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if printed := notices.String(); printed != "" {
		t.Errorf("publishing printed %q, want this process to name itself", printed)
	}
	if named := fmt.Sprintf("pid %d ", os.Getpid()); !strings.Contains(account, named) {
		t.Errorf("account = %q, want %q in it", account, named)
	}
}

// TestASweepThatFailsIsReportedAndTheLockStillGranted pins the sweep's place
// beside the publish it precedes: housekeeping on a diagnostic, so a failure
// is loud but costs neither the lock nor this holder's own record.
func TestASweepThatFailsIsReportedAndTheLockStillGranted(t *testing.T) {
	notices := captureLockNotices(t, time.Hour, time.Hour)
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	storageDir := storageDirOf(lockPath)
	// A directory under a record's name: the probe cannot open it as a file,
	// so the sweep fails on it.
	if err := os.MkdirAll(filepath.Join(lockHolderDir(storageDir, lockPath), lockHolderRecordPrefix+"unprobeable"), 0o755); err != nil {
		t.Fatalf("mkdir unprobeable record: %v", err)
	}

	release, err := acquireStoreLock(context.Background(), storageDir, lockPath, true, 0)
	if err != nil {
		t.Fatalf("acquireStoreLock() error = %v, want the lock despite a failed sweep", err)
	}
	account := describeLockHolders(storageDir, lockPath)
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if printed := notices.String(); !strings.Contains(printed, "could not retire the records of holders gone") || !strings.Contains(printed, "unprobeable") {
		t.Errorf("notices = %q, want the failed sweep reported, naming the record", printed)
	}
	if named := fmt.Sprintf("pid %d ", os.Getpid()); !strings.Contains(account, named) {
		t.Errorf("account = %q, want this holder named despite the failed sweep", account)
	}
}

// TestARecordThatWillNotRetireIsReportedAndTheReleaseStillLands pins the
// release's half of that demotion: a record this release cannot remove costs a
// notice, never the release, and the lock's own release result comes back
// untouched — neither swallowed nor joined with the record's failure.
func TestARecordThatWillNotRetireIsReportedAndTheReleaseStillLands(t *testing.T) {
	notices := captureLockNotices(t, time.Hour, time.Hour)
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	storageDir := storageDirOf(lockPath)
	dir := lockHolderDir(storageDir, lockPath)
	lockErr := errors.New("lock release result")
	release := recordLockHolder(context.Background(), storageDir, lockPath, func() error { return lockErr })

	// The swept name becomes a non-empty directory the release cannot remove,
	// as on Windows it cannot remove a name a concurrent sweep's probe holds
	// open.
	var record string
	for _, name := range holderDirEntries(t, lockPath) {
		if strings.HasPrefix(name, lockHolderRecordPrefix) {
			record = filepath.Join(dir, name)
		}
	}
	if record == "" {
		t.Fatal("no holder record was published")
	}
	if err := os.Remove(record); err != nil {
		t.Fatalf("remove record name: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(record, "occupant"), 0o755); err != nil {
		t.Fatalf("occupy record name: %v", err)
	}

	if err := release(); err != lockErr {
		t.Fatalf("release() = %v, want exactly the lock's own release result", err)
	}
	if printed := notices.String(); !strings.Contains(printed, "could not retire this process's holder record") {
		t.Errorf("notices = %q, want the failed retirement reported", printed)
	}
}

// TestHoldingARetiredRecordFailsRatherThanPublishingAnImpostor pins the
// identity check on the hold. A record removed from under its private name
// before its hold landed must not be held and published anyway: filelock
// recreates the name empty, and that file would go out as this holder's
// record while naming nobody — costing the account a holder that is very much
// alive, silently. Nor may the refused file outlive the refusal: nothing
// sweeps a private name, so whatever the hold leaves there is never collected.
func TestHoldingARetiredRecordFailsRatherThanPublishingAnImpostor(t *testing.T) {
	t.Parallel()
	private := filepath.Join(t.TempDir(), lockHolderPrivatePrefix+"gone")
	payload := []byte(`{"pid":1}`)
	if err := writeNewRecord(private, payload); err != nil {
		t.Fatalf("writeNewRecord: %v", err)
	}
	if err := os.Remove(private); err != nil {
		t.Fatalf("retire the written record: %v", err)
	}

	release, err := holdWrittenRecord(private, payload)
	if err == nil {
		_ = release()
		t.Fatal("holdWrittenRecord() on a record retired before its hold returned no error")
	}
	if !strings.Contains(err.Error(), "no longer the record this process wrote") {
		t.Errorf("holdWrittenRecord() error = %v, want it to name the swapped record", err)
	}
	if _, err := os.Stat(private); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("holdWrittenRecord() left the refused file under its private name: stat err = %v, want not-exist", err)
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
	holder, err := acquireStoreLock(ctx, workspaceStorageDir(doltRoot), WorkspaceLockPath(doltRoot), true, 0)
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

// TestCommitContentionNamesTheHolder pins the account reaching the operator
// through the commit lock — the busiest of the three, and the one whose only
// other coverage feeds wrapCommitLockContention a bare sentinel that carries
// no account at all. Dropping the %w in either wrap would leave that coverage
// green while every commit-contention message stopped naming its holder.
//
// Serial by construction: it shrinks the production waits the commit lock's
// budget is derived from, which are package state. [LAW:no-shared-mutable-globals]
func TestCommitContentionNamesTheHolder(t *testing.T) {
	wait, closeReserve := coResidentHolderWait, rotationCloseReserve
	coResidentHolderWait, rotationCloseReserve = 0, 0
	t.Cleanup(func() { coResidentHolderWait, rotationCloseReserve = wait, closeReserve })

	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	lockPath := commitLockPathForDolt(doltRoot)
	holder, err := acquireStoreLock(ctx, workspaceStorageDir(doltRoot), lockPath, true, 0)
	if err != nil {
		t.Fatalf("holder acquireStoreLock() error = %v", err)
	}
	defer func() {
		if err := holder(); err != nil {
			t.Errorf("release holder: %v", err)
		}
	}()

	_, err = LockCommitPath(ctx, doltRoot)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("LockCommitPath() error = %v, want ErrWorkspaceBusy", err)
	}
	if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.Contains(err.Error(), want) {
		t.Errorf("LockCommitPath() error %q does not carry %q", err, want)
	}
}

// TestBeaconContentionNamesTheSquatter pins the one wrapper that builds its own
// message rather than carrying the account out. A foreign process holding the
// beacon past every probe window is the case where naming it matters most. The
// sentinel stays un-propagated — that classification is deliberate — so the
// account has to travel as text, and both halves are pinned here together.
func TestBeaconContentionNamesTheSquatter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	squatter, err := acquireStoreLock(ctx, workspaceStorageDir(doltRoot), MirrorBeaconLockPath(doltRoot), true, 0)
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

// stepClock is a clock a test owns for one wait. Its first reading is the
// epoch and each later one is a step past the one before; tick runs with a
// reading's index before that reading is handed out, so whatever a test does
// at a tick lands exactly between two of the policy's polls. The contender
// polls on its own goroutine, which is the test's, so no reading races.
//
// [LAW:no-ambient-temporal-coupling] Every gap the policy measures is a
// whole number of steps however slowly the machine runs. A policy that never
// gives up would poll forever, so past stepClockReadLimit readings the clock
// fails the test instead.
type stepClock struct {
	t       *testing.T
	step    time.Duration
	tick    func(read int)
	reads   int
	instant time.Time
}

const stepClockReadLimit = 100

func newStepClock(t *testing.T, step time.Duration, tick func(read int)) *stepClock {
	return &stepClock{t: t, step: step, tick: tick, instant: time.Unix(0, 0)}
}

func (c *stepClock) now() time.Time {
	if c.reads == stepClockReadLimit {
		c.t.Fatalf("the wait was still polling after %d readings (%s); it never gave up", c.reads, c.elapsed())
	}
	if c.reads > 0 {
		c.instant = c.instant.Add(c.step)
	}
	c.tick(c.reads)
	c.reads++
	return c.instant
}

// elapsed is the span from the first reading to the latest.
func (c *stepClock) elapsed() time.Duration { return c.instant.Sub(time.Unix(0, 0)) }

func noTick(int) {}

// TestWaitOutlastsHoldersThatKeepChanging pins holdWait's rule through the
// acquisition every contender runs: the wait counts from the last holder to
// arrive at the lock, not from the contender's arrival. Shared holders hand
// the lock along for far longer than the wait, each acquiring before the last
// releases so the lock is never free, and the contender must keep waiting for
// as long as they do — the queue of short holders that sixteen concurrent
// `lit new` are, waited out rather than refused. A pure elapsed budget gives
// up on this contender at the wait. Once the last holder stands still the
// contender gives up exactly one wait after that holder arrived: no sooner,
// which shows the clock restarted on that arrival, and no later.
//
// The holders change at the contender's own polls, by its clock, so every
// gap between arrivals is exactly one poll however slowly the machine runs.
func TestWaitOutlastsHoldersThatKeepChanging(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	const (
		wait  = 300 * time.Millisecond
		steps = 8
	)

	holder, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, false, 0)
	if err != nil {
		t.Fatalf("first holder: %v", err)
	}
	defer func() {
		if err := holder(); err != nil {
			t.Errorf("release last holder: %v", err)
		}
	}()
	clock := newStepClock(t, storeLockPollInterval, func(read int) {
		if read == 0 || read > steps {
			return
		}
		next, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, false, 0)
		if err != nil {
			t.Fatalf("holder %d: %v", read+1, err)
		}
		previous := holder
		holder = next
		if err := previous(); err != nil {
			t.Fatalf("release holder %d: %v", read, err)
		}
	})

	_, err = acquireStoreLockOn(ctx, clock.now, storageDirOf(lockPath), lockPath, true, wait)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("contended acquireStoreLockOn() error = %v, want ErrWorkspaceBusy once the last holder stood still", err)
	}
	lastArrival := steps * storeLockPollInterval
	if got := clock.elapsed(); got < lastArrival {
		t.Fatalf("the contender gave up %s in, with a new holder arriving every %s against a %s wait; it gave up on holders that were making progress", got, storeLockPollInterval, wait)
	}
	if got := clock.elapsed(); got != lastArrival+wait {
		t.Fatalf("the contender gave up %s in, want %s: one %s wait after the last holder arrived at %s", got, lastArrival+wait, wait, lastArrival)
	}
}

// TestWaitFailsOnceHoldersStandStill pins the other edge of the same rule: a
// holder that does not move fails the contender once the wait has elapsed —
// no sooner, and without waiting out a second wait for a holder that was
// never going to change.
func TestWaitFailsOnceHoldersStandStill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	const wait = 200 * time.Millisecond

	holder, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, true, 0)
	if err != nil {
		t.Fatalf("holder acquireStoreLock() error = %v", err)
	}
	defer func() {
		if err := holder(); err != nil {
			t.Errorf("release holder: %v", err)
		}
	}()

	clock := newStepClock(t, storeLockPollInterval, noTick)
	_, err = acquireStoreLockOn(ctx, clock.now, storageDirOf(lockPath), lockPath, true, wait)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("contended acquireStoreLockOn() error = %v, want ErrWorkspaceBusy", err)
	}
	if got := clock.elapsed(); got != wait {
		t.Fatalf("contender gave up %s in against a %s wait; a holder standing still fails it at the wait", got, wait)
	}
}

// TestWaitUnderAnUnrecordedHolderIsTheElapsedBudget pins what the rule
// reduces to when there is nothing to read: a holder that left no record — a
// foreign process, a lit older than holder records — contributes no name and
// no progress, so the contender fails at the wait, exactly as under a
// recorded holder that stands still.
func TestWaitUnderAnUnrecordedHolderIsTheElapsedBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	const wait = 200 * time.Millisecond

	release, acquired, err := filelock.Acquire(ctx, lockPath, true, 1, 0)
	if err != nil || !acquired {
		t.Fatalf("take lock as an unrecorded holder: acquired=%v err=%v", acquired, err)
	}
	defer func() { _ = release() }()

	clock := newStepClock(t, storeLockPollInterval, noTick)
	_, err = acquireStoreLockOn(ctx, clock.now, storageDirOf(lockPath), lockPath, true, wait)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("contended acquireStoreLockOn() error = %v, want ErrWorkspaceBusy", err)
	}
	if got := clock.elapsed(); got != wait {
		t.Fatalf("contender gave up %s in against a %s wait", got, wait)
	}
	if !strings.Contains(err.Error(), "left no record") {
		t.Fatalf("contention error %q does not say the holder left no record", err)
	}
}

// TestWriteEngineRecordsItselfAsTheJournalHolder pins that a write engine's
// hold on Dolt's LOCK is named the way every lit-minted lock's hold is: the
// record exists for exactly the engine's life, and names this process, so a
// contender that fails against a live `lit new` can say so.
func TestWriteEngineRecordsItselfAsTheJournalHolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	s, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	holders, problems := readLockHolders(workspaceStorageDir(doltRoot), DoltJournalLockPath(doltRoot))
	if len(problems) != 0 {
		t.Fatalf("reading the journal holders: %v", problems)
	}
	if len(holders) != 1 || holders[0].PID != os.Getpid() {
		t.Fatalf("journal holders while a write engine is open = %+v, want exactly this process", holders)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	holders, problems = readLockHolders(workspaceStorageDir(doltRoot), DoltJournalLockPath(doltRoot))
	if len(problems) != 0 {
		t.Fatalf("reading the journal holders after Close: %v", problems)
	}
	if len(holders) != 0 {
		t.Fatalf("journal holders after Close = %+v, want none; a record outliving its engine names a departed holder", holders)
	}

	// A read engine records nothing: it cannot know whether it took LOCK or
	// fell back past it, and a record naming a holder that holds nothing
	// would send an operator after the wrong pid.
	r, err := OpenForRead(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenForRead() error = %v", err)
	}
	defer r.Close()
	if holders, _ := readLockHolders(workspaceStorageDir(doltRoot), DoltJournalLockPath(doltRoot)); len(holders) != 0 {
		t.Fatalf("journal holders while a read engine is open = %+v, want none", holders)
	}
}

// TestASweepDoesNotRestartTheWait pins that only an arrival is progress. A
// holder dies mid-wait without retiring its record; the contender's own
// notice sweeps the record, and that departure must not restart the
// clock — a contender whose wait is prolonged by its own housekeeping fails
// later than the contract says, and by an amount nobody can predict.
func TestASweepDoesNotRestartTheWait(t *testing.T) {
	captureLockNotices(t, 0, 0)
	ctx := context.Background()
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	const wait = 300 * time.Millisecond

	holder, err := acquireStoreLock(ctx, storageDirOf(lockPath), lockPath, true, 0)
	if err != nil {
		t.Fatalf("holder acquireStoreLock() error = %v", err)
	}
	defer func() {
		if err := holder(); err != nil {
			t.Errorf("release holder: %v", err)
		}
	}()
	// A second holder's record, live for as long as the test holds its
	// flock, so the contender's first reading of who is there includes it.
	dying := filepath.Join(lockHolderDir(storageDirOf(lockPath), lockPath), lockHolderRecordPrefix+"99999-1")
	if err := os.WriteFile(dying, []byte(`{"pid":99999,"command":"lit dying","since":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("plant record: %v", err)
	}
	dies, alive, err := filelock.Acquire(ctx, dying, true, 1, 0)
	if err != nil || !alive {
		t.Fatalf("hold the planted record: acquired=%v err=%v", alive, err)
	}

	// At the first poll the holder dies without retiring its record, and that
	// poll's notice (due at every poll here) sweeps it; by the second poll the
	// record must be gone, so the sweep is proven to land mid-wait rather than
	// in the refusal's own holder account at the end.
	clock := newStepClock(t, storeLockPollInterval, func(read int) {
		switch read {
		case 1:
			if err := dies(); err != nil {
				t.Fatalf("release the planted record: %v", err)
			}
		case 2:
			if _, statErr := os.Stat(dying); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("the dead holder's record was not swept during the wait (stat: %v); the test did not exercise a sweep", statErr)
			}
		}
	})
	_, err = acquireStoreLockOn(ctx, clock.now, storageDirOf(lockPath), lockPath, true, wait)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("contended acquireStoreLockOn() error = %v, want ErrWorkspaceBusy", err)
	}
	if got := clock.elapsed(); got != wait {
		t.Fatalf("contender gave up %s in against a %s wait; the sweep of a dead holder's record restarted its clock", got, wait)
	}
}
