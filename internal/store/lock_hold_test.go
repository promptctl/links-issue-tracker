package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/suiteenv"
)

// heldReports collects what a hold reports when it ends.
type heldReports struct {
	mu      sync.Mutex
	holds   []LockHold
	waits   []LockWait
	records []LockRecord
}

func (r *heldReports) observer() LockObserver {
	return LockObserver{
		Waited:   func(w LockWait) { r.mu.Lock(); r.waits = append(r.waits, w); r.mu.Unlock() },
		Held:     func(h LockHold) { r.mu.Lock(); r.holds = append(r.holds, h); r.mu.Unlock() },
		Recorded: func(rec LockRecord) { r.mu.Lock(); r.records = append(r.records, rec); r.mu.Unlock() },
	}
}

func (r *heldReports) recordsSnapshot() []LockRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]LockRecord(nil), r.records...)
}

func (r *heldReports) snapshot() ([]LockHold, []LockWait) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]LockHold(nil), r.holds...), append([]LockWait(nil), r.waits...)
}

// handTickedHold is the mirror hold of a test that drives a hold's heartbeat
// by hand: so long that the holder wait it derives, and the heartbeat a
// quarter of that, never fire inside the test.
const handTickedHold = time.Hour

// takenHold is a hold of kind access on a fresh store root, told by hand that
// Dolt took LOCK, with what it reports going to reports and what it prints to
// stderr.
func takenHold(t *testing.T, stderr io.Writer, access engineAccess, reports *heldReports) (*lockHold, func() string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "dolt")
	hold := newLockHold(WithLockObserver(context.Background(), reports.observer()), stderr, suiteenv.WithHold(handTickedHold), root, access, &longPhase{})
	hold.taken()
	t.Cleanup(hold.end)
	arrival := func() string {
		t.Helper()
		latest, err := latestArrival(hold.storageDir, hold.lockPath)
		if err != nil {
			t.Fatalf("latestArrival() error = %v", err)
		}
		return latest.name
	}
	if got := arrival(); got != hold.name() {
		t.Fatalf("arrival at LOCK taken = %q, want the hold's own name %q", got, hold.name())
	}
	return hold, arrival
}

// A routine hold announces its arrival the instant Dolt takes LOCK, then
// writes a heartbeat on every tick its engine worked since the last one,
// however short the work and however it fell between ticks, and none on a
// tick with no engine work. [LAW:no-ambient-temporal-coupling] the latch, not
// the tick's instant, decides.
func TestARoutineHoldHeartbeatsOnEveryTickItsEngineWorked(t *testing.T) {
	hold, arrival := takenHold(t, testStderr(), engineWriteRoutine, &heldReports{})
	name := arrival()
	if isHeartbeat(name) {
		t.Fatalf("arrival at LOCK taken = %q, want the hold's record-shaped name", name)
	}

	steps := []struct {
		does      string
		work      func()
		heartbeat bool
	}{
		{"a statement shorter than a tick, between two ticks", func() { hold.enter(); hold.exit() }, true},
		{"no engine work at all", func() {}, false},
		{"a statement still running at the tick", func() { hold.enter() }, true},
		{"the same statement, still running", func() {}, true},
		{"the statement ending before the tick", func() { hold.exit() }, true},
		{"no engine work after it", func() {}, false},
	}
	last := arrival()
	for _, step := range steps {
		step.work()
		hold.tick()
		got := arrival()
		if beat := got != last; beat != step.heartbeat {
			t.Fatalf("%s: heartbeat written = %t (arrival %q -> %q), want %t", step.does, beat, last, got, step.heartbeat)
		}
		if step.heartbeat && (!isHeartbeat(got) || !strings.HasPrefix(got, name)) {
			t.Fatalf("%s: heartbeat %q is not a heartbeat of %q", step.does, got, name)
		}
		last = got
	}
}

// The holder record a waiter's account prints carries the name the hold
// announced and heartbeats under, so one acquisition is one name wherever it
// is read, and it stands from the instant Dolt took LOCK.
func TestAHoldsRecordCarriesTheNameItAnnounced(t *testing.T) {
	hold, arrival := takenHold(t, testStderr(), engineWriteRoutine, &heldReports{})
	t.Cleanup(func() {
		if err := hold.retire(); err != nil {
			t.Errorf("retire record: %v", err)
		}
	})
	entries, err := os.ReadDir(lockHolderDir(hold.storageDir, hold.lockPath))
	if err != nil {
		t.Fatalf("read holder dir: %v", err)
	}
	var records []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), lockHolderRecordPrefix) {
			records = append(records, entry.Name())
		}
	}
	if want := arrival(); len(records) != 1 || records[0] != want {
		t.Fatalf("holder records = %q, want the one record the arrival %q names", records, want)
	}
}

// The long phase is the store's, not one engine's: an engine rotated inside
// it (a GC-contention reconnect in a migration's commit) opens into the phase
// and sends no heartbeat until the phase ends.
func TestALongPhaseOutlivesAConnectionRotation(t *testing.T) {
	env := envWithHold(handTickedHold)
	s, err := OpenRoutine(context.Background(), env, testStderr(), migratedDoltDir(t), "test-workspace-id")
	if err != nil {
		t.Fatalf("OpenRoutine() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	leave := s.phase.enter()
	if err := s.reconnect(context.Background()); err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	arrival := func() string {
		t.Helper()
		latest, err := latestArrival(s.hold.storageDir, s.hold.lockPath)
		if err != nil {
			t.Fatalf("latestArrival() error = %v", err)
		}
		return latest.name
	}
	before := arrival()
	s.hold.enter()
	s.hold.exit()
	s.hold.tick()
	if got := arrival(); got != before {
		t.Fatalf("an engine rotated inside the long phase wrote heartbeat %q", got)
	}
	leave()
	s.hold.enter()
	s.hold.exit()
	s.hold.tick()
	if got := arrival(); got == before {
		t.Fatal("no heartbeat after the long phase ended")
	}
}

// A connection rotation rotates the holder record with the engine: one
// record stands after it, under the name the new hold announces.
func TestAReconnectRotatesTheHolderRecordWithTheEngine(t *testing.T) {
	s, err := Open(context.Background(), testEnv(), testStderr(), migratedDoltDir(t), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	before := s.hold.name()
	if err := s.reconnect(context.Background()); err != nil {
		t.Fatalf("reconnect() error = %v", err)
	}
	if s.hold.name() == before {
		t.Fatal("the reconnect kept the previous hold")
	}
	entries, err := os.ReadDir(lockHolderDir(s.hold.storageDir, s.hold.lockPath))
	if err != nil {
		t.Fatalf("read holder dir: %v", err)
	}
	var records []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), lockHolderRecordPrefix) {
			records = append(records, entry.Name())
		}
	}
	if len(records) != 1 || records[0] != s.hold.name() {
		t.Fatalf("holder records after reconnect = %q, want only the new hold's %q", records, s.hold.name())
	}
}

// A hold has one life, so a second Taken is said aloud, never dropped.
func TestASecondTakenOnOneHoldIsReported(t *testing.T) {
	t.Parallel()
	notices := &syncWriter{}
	hold, _ := takenHold(t, notices, engineWriteRoutine, &heldReports{})
	hold.taken()
	if printed := notices.String(); !strings.Contains(printed, "again under a hold that already took it") {
		t.Fatalf("notices = %q, want the second Taken reported", printed)
	}
}

// A long hold says nothing after its arrival, however its engine works: its
// waiters judge it by stand-still and fail naming it, as they always have.
func TestALongHoldNeverHeartbeats(t *testing.T) {
	reports := &heldReports{}
	hold, arrival := takenHold(t, testStderr(), engineWrite, reports)
	name := arrival()
	hold.enter()
	hold.tick()
	hold.exit()
	hold.tick()
	if got := arrival(); got != name {
		t.Fatalf("a long hold wrote %q after its arrival %q", got, name)
	}
	hold.end()
	holds, _ := reports.snapshot()
	if len(holds) != 1 || holds[0].Routine || holds[0].Heartbeats != 1 {
		t.Fatalf("long hold reported %+v, want one non-routine hold with only its arrival written", holds)
	}
}

// A schema-changing migration inside a routine hold is a long phase: no
// heartbeat while it runs, and they resume when it is done.
func TestASuspendedRoutineHoldSendsNoHeartbeat(t *testing.T) {
	hold, arrival := takenHold(t, testStderr(), engineWriteRoutine, &heldReports{})
	resume := hold.phase.enter()
	before := arrival()
	hold.enter()
	hold.exit()
	hold.tick()
	if got := arrival(); got != before {
		t.Fatalf("a suspended hold wrote heartbeat %q", got)
	}
	resume()
	hold.enter()
	hold.exit()
	hold.tick()
	if got := arrival(); got == before {
		t.Fatal("a resumed hold wrote no heartbeat after engine work")
	}
}

// A hold ends once, reports once, and writes nothing after: no heartbeat
// stands for a lock nobody holds.
func TestAnEndedHoldReportsOnceAndWritesNothingMore(t *testing.T) {
	reports := &heldReports{}
	hold, arrival := takenHold(t, testStderr(), engineWriteRoutine, reports)
	hold.enter()
	hold.exit()
	hold.tick()
	hold.end()
	hold.end()
	after := arrival()
	hold.enter()
	hold.exit()
	hold.tick()
	if got := arrival(); got != after {
		t.Fatalf("an ended hold wrote %q", got)
	}
	holds, _ := reports.snapshot()
	if len(holds) != 1 {
		t.Fatalf("hold reported %d times, want once", len(holds))
	}
	if got := holds[0]; !got.Routine || got.Heartbeats != 2 || got.HeartbeatFailures != 0 || got.Lock != "LOCK" {
		t.Fatalf("hold reported %+v, want a routine LOCK hold with its arrival and one heartbeat written", got)
	}
}

// A hold Dolt never gave LOCK held nothing, so it reports nothing: a read
// engine's, or an open that lost the race.
func TestAHoldThatNeverTookTheLockReportsNothing(t *testing.T) {
	reports := &heldReports{}
	hold := newLockHold(WithLockObserver(context.Background(), reports.observer()), testStderr(), suiteenv.Deadlines(), filepath.Join(t.TempDir(), "dolt"), engineWriteRoutine, &longPhase{})
	hold.enter()
	hold.exit()
	hold.end()
	if holds, _ := reports.snapshot(); len(holds) != 0 {
		t.Fatalf("an untaken hold reported %+v", holds)
	}
}

// contend runs work on a store opened by open while a write open contends for
// LOCK, and returns the contender's open error and how long it took. The
// holder's open runs under reports' observer.
//
// The contender opens under the suite's long deadlines, whatever the
// holder's: it judges the holder by the allowance the holder states, so a
// test sets the wait by the deadlines it hands the holder.
func contend(t *testing.T, open func(context.Context, string) (*Store, error), work func(*Store) error, reports *heldReports) (error, time.Duration) {
	t.Helper()
	ctx := WithLockObserver(context.Background(), reports.observer())
	doltRoot := migratedDoltDir(t)
	holder, err := open(ctx, doltRoot)
	if err != nil {
		t.Fatalf("holder open error = %v", err)
	}
	holderDone := make(chan error, 1)
	go func() { holderDone <- errors.Join(work(holder), holder.Close()) }()

	start := time.Now()
	contender, err := Open(ctx, testEnv(), testStderr(), doltRoot, "test-workspace-id")
	elapsed := time.Since(start)
	if err == nil {
		err = contender.Close()
	}
	if holderErr := <-holderDone; holderErr != nil {
		t.Fatalf("holder error = %v", holderErr)
	}
	return err, elapsed
}

// envWithHold is the suite's environment under suiteenv.WithHold.
func envWithHold(hold time.Duration) environ.Env {
	return envWithDeadlines(suiteenv.WithHold(hold))
}

// The ticket's bar: a routine holder whose engine work outlasts the wait many
// times over is waited out, and the writer behind it lands. On master the
// writer failed workspace_busy one wait into the holder's statement.
func TestAWriterWaitsOutARoutineHolderWhoseEngineIsWorking(t *testing.T) {
	// serial: no t.Parallel — asserts a wall-clock bound against the holder
	// wait, which parallel load would move.
	env := envWithDeadlines(suiteenv.Contention())
	statement := 5 * env.Deadlines().HolderWait()
	reports := &heldReports{}
	err, elapsed := contend(t, func(ctx context.Context, root string) (*Store, error) {
		return OpenRoutine(ctx, env, testStderr(), root, "test-workspace-id")
	}, func(s *Store) error {
		return s.ExecRawForTest(context.Background(), sqlSleep(statement))
	}, reports)
	if err != nil {
		t.Fatalf("a writer behind a working routine holder failed after %s: %v", elapsed, err)
	}
	if elapsed < 4*env.Deadlines().HolderWait() {
		t.Fatalf("the writer opened after %s, before the holder's %s statement could have ended; it never waited", elapsed, statement)
	}
	holds, waits := reports.snapshot()
	var routine *LockHold
	for i := range holds {
		if holds[i].Routine {
			routine = &holds[i]
		}
	}
	if routine == nil || routine.Heartbeats < 4 || routine.Executing < time.Second {
		t.Fatalf("holds reported %+v, want the routine holder's, heartbeating through its %s statement", holds, statement)
	}
	if lockWait := lockWaitOn(reports, "LOCK"); lockWait == nil || lockWait.Outcome != LockWaitAcquired || lockWait.ProgressSeen < 2 {
		t.Fatalf("waits reported %+v, want the writer's LOCK wait acquired after seeing the holder's heartbeats", waits)
	}
}

// The compaction backstop's hold is routine: DOLT_GC is engine work, so a
// writer behind a pass that outlasts the wait lands rather than failing behind
// housekeeping it never asked for.
func TestAWriterWaitsOutAMaintenanceHold(t *testing.T) {
	// serial: no t.Parallel — its holder must outlast the holder wait by a
	// wall-clock margin, which parallel load would eat.
	env := envWithDeadlines(suiteenv.Contention())
	reports := &heldReports{}
	err, elapsed := contend(t, func(ctx context.Context, root string) (*Store, error) {
		return OpenMaintenance(ctx, env, testStderr(), root, "test-workspace-id")
	}, func(s *Store) error {
		return s.ExecRawForTest(context.Background(), sqlSleep(5*env.Deadlines().HolderWait()))
	}, reports)
	if err != nil {
		t.Fatalf("a writer behind a working maintenance hold failed after %s: %v", elapsed, err)
	}
	if lockWait := lockWaitOn(reports, "LOCK"); lockWait == nil || lockWait.Outcome != LockWaitAcquired || lockWait.ProgressSeen < 2 {
		t.Fatalf("the writer's LOCK wait was reported as %+v, want acquired after seeing the holder's heartbeats", lockWait)
	}
}

// The same engine work under a long hold fails the writer one wait in, naming
// the holder: long holds keep the contract they always had.
func TestAWriterBehindALongHolderStillFailsNamingIt(t *testing.T) {
	// serial: no t.Parallel — asserts a wall-clock bound against the holder
	// wait, which parallel load would move.
	env := envWithDeadlines(suiteenv.Contention())
	wait := env.Deadlines().HolderWait()
	reports := &heldReports{}
	err, elapsed := contend(t, func(ctx context.Context, root string) (*Store, error) {
		return Open(WithHolderCommand(ctx, "lit import"), env, testStderr(), root, "test-workspace-id")
	}, func(s *Store) error {
		return s.ExecRawForTest(context.Background(), sqlSleep(3*wait))
	}, reports)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("a writer behind a long holder got %v after %s, want ErrWorkspaceBusy", err, elapsed)
	}
	if !strings.Contains(err.Error(), "(lit import)") {
		t.Fatalf("refusal %q does not name the long holder", err)
	}
	if elapsed > 2*wait {
		t.Fatalf("the writer failed after %s; a long holder must fail it about one %s wait in", elapsed, wait)
	}
	if wait := lockWaitOn(reports, "LOCK"); wait == nil || wait.Outcome != LockWaitStoodStill {
		t.Fatalf("the writer's LOCK wait was reported as %+v, want stood_still", wait)
	}
}

// sqlSleep is a statement that keeps its engine working for d.
func sqlSleep(d time.Duration) string {
	return fmt.Sprintf("SELECT SLEEP(%.3f)", d.Seconds())
}

// lockWaitOn is the last wait reports recorded on the lock named lock.
func lockWaitOn(reports *heldReports, lock string) *LockWait {
	_, waits := reports.snapshot()
	var found *LockWait
	for i := range waits {
		if waits[i].Lock == lock {
			found = &waits[i]
		}
	}
	return found
}

// cancelAtNotice is an operator channel that cancels a context the first
// time a wait on one lock reports itself there: a notice the wait prints only
// from inside its wait (holdWait.NextBackOff), so the cancel lands while the
// writer is known to be waiting on that lock, however slowly it got there.
type cancelAtNotice struct {
	mu     sync.Mutex
	said   strings.Builder
	notice string
	cancel context.CancelFunc
}

func (w *cancelAtNotice) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.said.Write(p)
	if strings.Contains(w.said.String(), w.notice) {
		w.cancel()
	}
	return os.Stderr.Write(p)
}

// A writer whose context ends while it waits on a holder was cancelled, not
// refused: Dolt's open hands back its lock error either way, and the wait
// still says which it was.
func TestAWriterCancelledBehindAHolderReportsItsWaitCancelled(t *testing.T) {
	// The holder states the suite's allowance, far past the notice grace, so
	// the context, cancelled at the writer's first notice of its LOCK wait,
	// ends the wait, not the allowance.
	t.Parallel()
	doltRoot := migratedDoltDir(t)
	holder, err := Open(context.Background(), testEnv(), testStderr(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("holder Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := holder.Close(); err != nil {
			t.Errorf("holder Close() error = %v", err)
		}
	})
	reports := &heldReports{}
	ctx, cancel := context.WithCancel(WithLockObserver(context.Background(), reports.observer()))
	defer cancel()
	stderr := &cancelAtNotice{notice: "still waiting for " + DoltJournalLockPath(doltRoot), cancel: cancel}
	if contender, err := Open(ctx, testEnv(), stderr, doltRoot, "test-workspace-id"); err == nil {
		_ = contender.Close()
		t.Fatal("a writer opened beside a live holder")
	}
	if wait := lockWaitOn(reports, "LOCK"); wait == nil || wait.Outcome != LockWaitCancelled {
		_, waits := reports.snapshot()
		t.Fatalf("the cancelled writer's LOCK wait was reported as %+v, want cancelled; every wait reported: %+v", wait, waits)
	}
}

// A wait the policy did not give up on, whose context is live, and whose
// acquisition failed, failed: neither refused nor cancelled.
func TestAWaitWhoseAttemptFailedReportsFailed(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "test.lock")
	w := newHoldWait(testStderr(), operatorNotices(), storageDirOf(lockPath), lockPath, fixedPatience(time.Hour), time.Now)
	w.Reset()
	w.NextBackOff()
	if wait, waited := w.report(context.Background(), errors.New("disk on fire")); !waited || wait.Outcome != LockWaitFailed {
		t.Fatalf("report() = %+v, %t; want a failed wait", wait, waited)
	}
}

// A bootstrap's engine holds LOCK like any write engine, so its hold is
// reported like any other.
func TestABootstrapEnginesHoldIsReported(t *testing.T) {
	reports := &heldReports{}
	ctx := WithLockObserver(context.Background(), reports.observer())
	if err := buildDoltRoot(ctx, testEnv(), testStderr(), filepath.Join(t.TempDir(), "staging"), "test-workspace-id"); err != nil {
		t.Fatalf("buildDoltRoot() error = %v", err)
	}
	if holds, _ := reports.snapshot(); len(holds) != 2 {
		t.Fatalf("bootstrap reported holds %+v, want one per engine it opened", holds)
	}
}

// A routine holder outside its engine (at a prompt, on the network, waiting
// on a lock, doing CLI work) is not working, and its writers fail one wait
// after its last engine work, naming it.
func TestAWriterBehindARoutineHolderOutsideItsEngineFails(t *testing.T) {
	// serial: no t.Parallel — its holder must outlast the holder wait by a
	// wall-clock margin, which parallel load would eat.
	env := envWithDeadlines(suiteenv.Contention())
	reports := &heldReports{}
	err, elapsed := contend(t, func(ctx context.Context, root string) (*Store, error) {
		return OpenRoutine(WithHolderCommand(ctx, "lit purge"), env, testStderr(), root, "test-workspace-id")
	}, func(*Store) error {
		time.Sleep(3 * env.Deadlines().HolderWait())
		return nil
	}, reports)
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("a writer behind an idle routine holder got %v after %s, want ErrWorkspaceBusy", err, elapsed)
	}
	if !strings.Contains(err.Error(), "(lit purge)") {
		t.Fatalf("refusal %q does not name the routine holder", err)
	}
	if wait := lockWaitOn(reports, "LOCK"); wait == nil || wait.Outcome != LockWaitStoodStill {
		t.Fatalf("the writer's LOCK wait was reported as %+v, want stood_still", wait)
	}
}

// The refusal's clause about the lock's latest arrival says which kind it
// was: an acquisition announcing itself, or a working holder's heartbeat.
func TestTheHolderAccountTellsAHeartbeatFromAnAcquisition(t *testing.T) {
	storageDir := t.TempDir()
	lockPath := filepath.Join(storageDir, "LOCK")
	dir := lockHolderDir(storageDir, lockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, want string }{
		{"holder-1-abc-1", "the last lit acquisition announced itself"},
		{"holder-1-abc-1.3", "the holder's last heartbeat was"},
	} {
		if err := announceArrival(dir, c.name, suiteHolder()); err != nil {
			t.Fatal(err)
		}
		if got := describeArrival(storageDir, lockPath); !strings.HasPrefix(got, c.want) {
			t.Errorf("arrival %q described as %q, want it to start %q", c.name, got, c.want)
		}
	}
}

// A heartbeat is only progress if a waiter can read it, so the arrival is
// never empty while it is rewritten: a waiter polling while a holder writes
// reads the last name or the next one, never nothing. A shorter name written
// over a longer one reads back exactly, with nothing of the old one left.
func TestAnArrivalIsNeverReadEmptyWhileItIsRewritten(t *testing.T) {
	storageDir := t.TempDir()
	lockPath := filepath.Join(storageDir, "LOCK")
	dir := lockHolderDir(storageDir, lockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := announceArrival(dir, "holder-1-abc-10", suiteHolder()); err != nil {
		t.Fatal(err)
	}
	if err := announceArrival(dir, "holder-1-abc-1", suiteHolder()); err != nil {
		t.Fatal(err)
	}
	if latest, err := latestArrival(storageDir, lockPath); err != nil || latest.name != "holder-1-abc-1" {
		t.Fatalf("latestArrival() = %q, %v; want the shorter name exactly", latest.name, err)
	}

	done := make(chan struct{})
	writeErr := make(chan error, 1)
	go func() {
		defer close(writeErr)
		for seq := 1; ; seq++ {
			select {
			case <-done:
				return
			default:
			}
			if err := announceArrival(dir, fmt.Sprintf("holder-1-abc-1.%d", seq), suiteHolder()); err != nil {
				writeErr <- err
				return
			}
		}
	}()
	for range 20000 {
		latest, err := latestArrival(storageDir, lockPath)
		if err != nil {
			t.Fatalf("latestArrival() error = %v", err)
		}
		if latest.name == "" {
			close(done)
			t.Fatal("a waiter read the arrival empty while a holder rewrote it; that heartbeat was invisible to it")
		}
	}
	close(done)
	if err := <-writeErr; err != nil {
		t.Fatalf("announceArrival() error = %v", err)
	}
}

// An arrival reads back as the name and the terms its holder wrote; a line
// that states no terms, as a lit older than them writes, reads as its name
// alone, still a name no earlier writing used. The longest line lit writes,
// a heartbeat of the largest pid and sequence stating no bound, fits the file.
func TestAnArrivalReadsBackTheTermsItsHolderStated(t *testing.T) {
	t.Parallel()
	storageDir := t.TempDir()
	lockPath := filepath.Join(storageDir, "LOCK")
	dir := lockHolderDir(storageDir, lockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	states := holderStatement{heartbeat: 575 * time.Millisecond, standStill: 2300 * time.Millisecond}
	if err := announceArrival(dir, "holder-1-abc-1.2", states); err != nil {
		t.Fatal(err)
	}
	if latest, err := latestArrival(storageDir, lockPath); err != nil || latest.name != "holder-1-abc-1.2" || !latest.stated || latest.states != states {
		t.Fatalf("latestArrival() = %+v, %v; want the name and the terms written", latest, err)
	}
	older := "holder-1-abc-2"
	if err := os.WriteFile(filepath.Join(dir, lockHolderArrivalName), []byte(older+strings.Repeat(" ", lockArrivalWidth-len(older))), 0o644); err != nil {
		t.Fatal(err)
	}
	if latest, err := latestArrival(storageDir, lockPath); err != nil || latest.name != older || latest.stated {
		t.Fatalf("latestArrival() of an older lit's line = %+v, %v; want its name and no terms", latest, err)
	}
	longest := heartbeatName(lockHolderRecordPrefix+"4194304-ffffffff-18446744073709551615", math.MaxUint64)
	if err := announceArrival(dir, longest, holderStatement{heartbeat: noStandStillBound, standStill: noStandStillBound}); err != nil {
		t.Fatalf("announceArrival() of the longest line lit writes: %v", err)
	}
}

// What an engine's hold of LOCK states: a routine hold its heartbeat and its
// run's allowance, a long hold the allowance and no heartbeat.
func TestALockHoldStatesItsHeartbeatAndAllowance(t *testing.T) {
	t.Parallel()
	d := suiteenv.WithHold(handTickedHold)
	for access, want := range map[engineAccess]holderStatement{
		engineWriteRoutine: {heartbeat: holdHeartbeatInterval(d), standStill: d.HolderWait()},
		engineWrite:        {standStill: d.HolderWait()},
	} {
		hold, _ := takenHold(t, testStderr(), access, &heldReports{})
		latest, err := latestArrival(hold.storageDir, hold.lockPath)
		if err != nil || !latest.stated || latest.states != want {
			t.Errorf("a %v hold's arrival = %+v, %v; want it to state %+v", access, latest, err, want)
		}
	}
}

// A name the arrival file cannot hold whole is refused, not cut short: a cut
// name could equal one written before and read as no progress.
func TestAnArrivalTooLongForTheFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := announceArrival(dir, strings.Repeat("x", lockArrivalWidth+1), suiteHolder()); err == nil {
		t.Fatal("announceArrival() accepted a name longer than the arrival file")
	}
}

// fakeConn is a driver connection whose transactions do nothing, so a test
// can hold one open through executingConn with no engine behind it.
type fakeConn struct{ driver.Conn }

func (fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) { return fakeTx{}, nil }

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

// A routine hold heartbeats on every tick while a transaction is open, with
// no statement running: a mutation computing between its statements (rank
// smoothing, measured at 1.9s under load) is the hold working. Once the
// transaction commits or rolls back, a tick with no work writes nothing.
func TestARoutineHoldHeartbeatsWhileATransactionIsOpen(t *testing.T) {
	for _, end := range []string{"commit", "rollback"} {
		hold, arrival := takenHold(t, testStderr(), engineWriteRoutine, &heldReports{})
		conn := executingConn{inner: fakeConn{}, hold: hold}
		tx, err := conn.BeginTx(context.Background(), driver.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		hold.tick() // clears the latch BeginTx's own call set
		for i := range 3 {
			last := arrival()
			hold.tick()
			if arrival() == last {
				t.Fatalf("%s: tick %d with the transaction open wrote no heartbeat", end, i+1)
			}
		}
		if end == "commit" {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		hold.tick() // the latch the transaction's end set
		last := arrival()
		hold.tick()
		if got := arrival(); got != last {
			t.Fatalf("%s: a tick after the transaction ended wrote %q", end, got)
		}
	}
}
