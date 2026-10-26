package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// heldReports collects what a hold reports when it ends.
type heldReports struct {
	mu    sync.Mutex
	holds []LockHold
	waits []LockWait
}

func (r *heldReports) observer() LockObserver {
	return LockObserver{
		Waited: func(w LockWait) { r.mu.Lock(); r.waits = append(r.waits, w); r.mu.Unlock() },
		Held:   func(h LockHold) { r.mu.Lock(); r.holds = append(r.holds, h); r.mu.Unlock() },
	}
}

func (r *heldReports) snapshot() ([]LockHold, []LockWait) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]LockHold(nil), r.holds...), append([]LockWait(nil), r.waits...)
}

// liftWaitOutOfPlay sets coResidentHolderWait so long that a hold's own
// heartbeat ticker never fires inside a test, which then drives tick by hand.
func liftWaitOutOfPlay(t *testing.T) {
	t.Helper()
	prev := coResidentHolderWait
	coResidentHolderWait = time.Hour
	t.Cleanup(func() { coResidentHolderWait = prev })
}

// takenHold is a hold of kind access on a fresh store root, told by hand that
// Dolt took LOCK, with what it reports going to reports.
func takenHold(t *testing.T, access engineAccess, reports *heldReports) (*lockHold, func() string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "dolt")
	hold := newLockHold(WithLockObserver(context.Background(), reports.observer()), root, access)
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
	return hold, arrival
}

// A routine hold announces its arrival the instant Dolt takes LOCK, then
// writes a heartbeat on every tick its engine worked since the last one,
// however short the work and however it fell between ticks, and none on a
// tick with no engine work. [LAW:no-ambient-temporal-coupling] the latch, not
// the tick's instant, decides.
func TestARoutineHoldHeartbeatsOnEveryTickItsEngineWorked(t *testing.T) {
	liftWaitOutOfPlay(t)
	hold, arrival := takenHold(t, engineWriteRoutine, &heldReports{})
	name := arrival()
	if name == "" || strings.Contains(name, ".") {
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
		if step.heartbeat && !strings.HasPrefix(got, name+".") {
			t.Fatalf("%s: heartbeat %q is not %q.<sequence>", step.does, got, name)
		}
		last = got
	}
}

// A long hold says nothing after its arrival, however its engine works: its
// waiters judge it by stand-still and fail naming it, as they always have.
func TestALongHoldNeverHeartbeats(t *testing.T) {
	liftWaitOutOfPlay(t)
	reports := &heldReports{}
	hold, arrival := takenHold(t, engineWrite, reports)
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
	liftWaitOutOfPlay(t)
	hold, arrival := takenHold(t, engineWriteRoutine, &heldReports{})
	resume := hold.suspend()
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
	liftWaitOutOfPlay(t)
	reports := &heldReports{}
	hold, arrival := takenHold(t, engineWriteRoutine, reports)
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
	hold := newLockHold(WithLockObserver(context.Background(), reports.observer()), filepath.Join(t.TempDir(), "dolt"), engineWriteRoutine)
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
func contend(t *testing.T, open func(context.Context, string) (*Store, error), work func(*Store) error, reports *heldReports) (error, time.Duration) {
	t.Helper()
	ctx := WithLockObserver(context.Background(), reports.observer())
	doltRoot := filepath.Join(t.TempDir(), "dolt")
	initial, err := Open(context.Background(), testEnv(), doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("initial Open() error = %v", err)
	}
	if err := initial.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	holder, err := open(ctx, doltRoot)
	if err != nil {
		t.Fatalf("holder open error = %v", err)
	}
	holderDone := make(chan error, 1)
	go func() { holderDone <- errors.Join(work(holder), holder.Close()) }()

	start := time.Now()
	contender, err := Open(ctx, testEnv(), doltRoot, "test-workspace-id")
	elapsed := time.Since(start)
	if err == nil {
		err = contender.Close()
	}
	if holderErr := <-holderDone; holderErr != nil {
		t.Fatalf("holder error = %v", holderErr)
	}
	return err, elapsed
}

// shrinkWait sets coResidentHolderWait for a contention test.
func shrinkWait(t *testing.T, wait time.Duration) {
	t.Helper()
	prev := coResidentHolderWait
	coResidentHolderWait = wait
	t.Cleanup(func() { coResidentHolderWait = prev })
}

// The ticket's bar: a routine holder whose engine work outlasts the wait many
// times over is waited out, and the writer behind it lands. On master the
// writer failed workspace_busy one wait into the holder's statement.
func TestAWriterWaitsOutARoutineHolderWhoseEngineIsWorking(t *testing.T) {
	// serial: rewrites the package-level coResidentHolderWait.
	shrinkWait(t, 400*time.Millisecond)
	reports := &heldReports{}
	err, elapsed := contend(t, func(ctx context.Context, root string) (*Store, error) {
		return OpenRoutine(ctx, testEnv(), root, "test-workspace-id")
	}, func(s *Store) error {
		return s.ExecRawForTest(context.Background(), "SELECT SLEEP(2)")
	}, reports)
	if err != nil {
		t.Fatalf("a writer behind a working routine holder failed after %s: %v", elapsed, err)
	}
	if elapsed < 4*coResidentHolderWait {
		t.Fatalf("the writer opened after %s, before the holder's 2s statement could have ended; it never waited", elapsed)
	}
	holds, waits := reports.snapshot()
	var routine *LockHold
	for i := range holds {
		if holds[i].Routine {
			routine = &holds[i]
		}
	}
	if routine == nil || routine.Heartbeats < 4 || routine.Executing < time.Second {
		t.Fatalf("holds reported %+v, want the routine holder's, heartbeating through its 2s statement", holds)
	}
	var lockWait *LockWait
	for i := range waits {
		if waits[i].Lock == "LOCK" {
			lockWait = &waits[i]
		}
	}
	if lockWait == nil || lockWait.Outcome != LockWaitAcquired || lockWait.ProgressSeen < 2 {
		t.Fatalf("waits reported %+v, want the writer's LOCK wait acquired after seeing the holder's heartbeats", waits)
	}
}

// The same engine work under a long hold fails the writer one wait in, naming
// the holder: long holds keep the contract they always had.
func TestAWriterBehindALongHolderStillFailsNamingIt(t *testing.T) {
	// serial: rewrites the package-level coResidentHolderWait.
	shrinkWait(t, 400*time.Millisecond)
	err, elapsed := contend(t, func(ctx context.Context, root string) (*Store, error) {
		return Open(WithHolderCommand(ctx, "lit import"), testEnv(), root, "test-workspace-id")
	}, func(s *Store) error {
		return s.ExecRawForTest(context.Background(), "SELECT SLEEP(2)")
	}, &heldReports{})
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("a writer behind a long holder got %v after %s, want ErrWorkspaceBusy", err, elapsed)
	}
	if !strings.Contains(err.Error(), "(lit import)") {
		t.Fatalf("refusal %q does not name the long holder", err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("the writer failed after %s; a long holder must fail it about one wait in", elapsed)
	}
}

// A routine holder outside its engine (at a prompt, on the network, waiting
// on a lock, doing CLI work) is not working, and its writers fail one wait
// after its last engine work, naming it.
func TestAWriterBehindARoutineHolderOutsideItsEngineFails(t *testing.T) {
	// serial: rewrites the package-level coResidentHolderWait.
	shrinkWait(t, 400*time.Millisecond)
	err, elapsed := contend(t, func(ctx context.Context, root string) (*Store, error) {
		return OpenRoutine(WithHolderCommand(ctx, "lit purge"), testEnv(), root, "test-workspace-id")
	}, func(*Store) error {
		time.Sleep(2 * time.Second)
		return nil
	}, &heldReports{})
	if !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("a writer behind an idle routine holder got %v after %s, want ErrWorkspaceBusy", err, elapsed)
	}
	if !strings.Contains(err.Error(), "(lit purge)") {
		t.Fatalf("refusal %q does not name the routine holder", err)
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
		if err := announceArrival(dir, c.name); err != nil {
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
	if err := announceArrival(dir, "holder-1-abc-10"); err != nil {
		t.Fatal(err)
	}
	if err := announceArrival(dir, "holder-1-abc-1"); err != nil {
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
			if err := announceArrival(dir, fmt.Sprintf("holder-1-abc-1.%d", seq)); err != nil {
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

// A name the arrival file cannot hold whole is refused, not cut short: a cut
// name could equal one written before and read as no progress.
func TestAnArrivalTooLongForTheFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := announceArrival(dir, strings.Repeat("x", lockArrivalWidth+1)); err == nil {
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
	liftWaitOutOfPlay(t)
	for _, end := range []string{"commit", "rollback"} {
		hold, arrival := takenHold(t, engineWriteRoutine, &heldReports{})
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
