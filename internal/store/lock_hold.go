package store

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dolthub/dolt/go/store/nbs"
	embedded "github.com/dolthub/driver"
)

// This file is the holder's half of a wait on Dolt's LOCK: what one write
// engine's hold of LOCK says about itself while it lasts. holdWait
// (lock_holder.go) is the waiter's half, and it reads only names.
//
// A waiter gives up once the holders in front of it have shown no progress
// for coResidentHolderWait. An acquisition is progress, so a queue of short
// holds is waited out however long it runs. A single hold showed progress
// once, at its arrival, and under a loaded machine one routine `lit new`
// stretches past the wait: Dolt's open after it takes LOCK, and the engine
// close, each ran for seconds with nothing for a waiter to read, and every
// writer behind it failed together. No wall-clock figure fixes that, because
// the slowdown is set by load lit does not control.
// [LAW:no-ambient-temporal-coupling]
//
// So a routine hold says it is working: while its engine executes, it
// rewrites LOCK's arrival file with a fresh name every holdHeartbeatInterval
// (a heartbeat), and the waiter's existing rule reads each one as progress.
// "Working" is the engine executing local work on the hold's behalf: its
// open after LOCK, a statement, a transaction from its begin to its commit,
// the close. Everything a holder can wait on outside itself (a person at a
// prompt, the network, another lock, a child process) happens outside the
// engine and outside any transaction, so it stops the heartbeat by
// construction, with no list of such waits to keep complete.
//
// Only a routine hold heartbeats (engineWriteRoutine): a command whose work
// does not grow with its input. A long hold (engineWrite) keeps the contract
// it always had: its waiters fail after the wait, naming it. A routine hold
// wedged inside a Dolt statement heartbeats like a slow one, because from
// outside the two are indistinguishable at any finite time; its waiters wait
// with a notice naming it every lockWaitNoticeInterval. That is the one place
// a writer's wait on LOCK has no bound, and it needs a Dolt bug; the
// bootstrap lock, which is not LOCK, waits out its builder with no bound by
// design (acquireBootstrapLock).

// LockWaitOutcome is how one acquisition's wait for a held lock ended.
type LockWaitOutcome string

const (
	// LockWaitAcquired: the lock was taken after the wait.
	LockWaitAcquired LockWaitOutcome = "acquired"
	// LockWaitStoodStill: the holders in front stood still for the whole
	// wait, and the contender gave up naming them.
	LockWaitStoodStill LockWaitOutcome = "stood_still"
	// LockWaitCancelled: the contender's context ended first.
	LockWaitCancelled LockWaitOutcome = "cancelled"
	// LockWaitFailed: the attempt failed for a reason other than the lock
	// being held, after the lock had been found held at least once.
	LockWaitFailed LockWaitOutcome = "failed"
)

// LockWait is one acquisition that found its lock held at least once: which
// lock, how long it waited (to the last poll that found the lock held; an
// acquisition lands within one poll interval after), how many holder names it saw arrive (each
// acquisition and each heartbeat of a working routine holder is one), and how
// it ended. An acquisition that found its lock free waited for nothing and
// reports nothing.
type LockWait struct {
	Lock         string
	Waited       time.Duration
	ProgressSeen int
	Outcome      LockWaitOutcome
}

// LockHold is one write engine's hold of Dolt's LOCK, reported when it ends:
// whether it was routine (heartbeating), how long it held LOCK, how much of
// that its engine was executing, the longest stretch it held LOCK without
// executing (the figure that says when a routine hold's non-engine work came
// near the wait), and the heartbeats it wrote and failed to write.
type LockHold struct {
	Lock              string
	Routine           bool
	Held              time.Duration
	Executing         time.Duration
	LongestIdle       time.Duration
	Heartbeats        int
	HeartbeatFailures int
}

// LockObserver is told about each lock wait and each LOCK hold of the work
// run under the context that carries it. The store reports these as values
// and never records them itself: the command's event is the CLI's, which
// installs one observer per invocation (invocationContext), as it does for
// Dolt's journal warnings. [LAW:effects-at-boundaries]
type LockObserver struct {
	Waited func(LockWait)
	Held   func(LockHold)
}

type lockObserverKey struct{}

// WithLockObserver returns ctx carrying observer. Both funcs must be set: a
// nil one panics here, where it was installed, rather than later inside a
// lock acquisition or Dolt's close.
func WithLockObserver(ctx context.Context, observer LockObserver) context.Context {
	if observer.Waited == nil || observer.Held == nil {
		panic("store.WithLockObserver: both Waited and Held must be set")
	}
	return context.WithValue(ctx, lockObserverKey{}, observer)
}

// lockObserverFrom is the observer ctx carries. Work run outside an
// invocation (a test, a tool) carries none and its waits and holds are
// reported to nobody, which is what running outside an invocation means.
func lockObserverFrom(ctx context.Context) LockObserver {
	if observer, ok := ctx.Value(lockObserverKey{}).(LockObserver); ok {
		return observer
	}
	return LockObserver{Waited: func(LockWait) {}, Held: func(LockHold) {}}
}

// holdHeartbeatInterval is how often a working routine hold says so: four
// times per coResidentHolderWait, so three heartbeats in a row can be lost to
// scheduling before a working holder looks still. Read live, like the wait,
// so a test that shrinks the wait shrinks the heartbeat with it.
// [LAW:one-source-of-truth] derived, never a second figure.
func holdHeartbeatInterval() time.Duration { return coResidentHolderWait / 4 }

// holdState is where a hold is in its one life: before Dolt took LOCK, while
// it holds it, and after it let it go (or the open failed). A hold never
// returns to an earlier state.
type holdState int

const (
	holdUnheld holdState = iota
	holdHeld
	holdEnded
)

// lockHold is one write engine's claim on Dolt's LOCK, built by the opener
// before the open and handed to Dolt as its journal lock observer
// (nbs.WithJournalLockObserver), so it knows the instants LOCK is taken and
// let go rather than the instants lit's own calls return. It also owns the
// wait the open makes for LOCK (wait), so one value reports both halves.
//
// Its record names are drawn once, when it is built: the arrival it
// announces, every heartbeat after it, and the holder record a waiter's
// account prints all carry the one name. [LAW:one-source-of-truth]
//
// [LAW:no-shared-mutable-globals] Every field below mu is written only under
// it, by Dolt's callbacks, the executing decorator, and the heartbeat ticker.
// The arrival file is written outside mu, since a write under a storm of
// readers can take a second and the engine's next statement must not wait on
// it. end stops the ticker and waits out any write it had begun, so nothing
// is written once end returns.
type lockHold struct {
	// stderr is the run's operator channel, where the hold reports what it
	// cannot do.
	stderr      io.Writer
	storageDir  string
	lockPath    string
	privatePath string
	recordPath  string
	access      engineAccess
	phase       *longPhase
	wait        *holdWait
	now         func() time.Time
	held        func(LockHold)
	waited      func(LockWait)

	mu        sync.Mutex
	state     holdState
	sequence  uint64
	executing int
	// worked latches "the engine ran since the last tick": set on every entry
	// into engine work and while any is running, cleared by each tick. A tick
	// that samples only "is it executing now" lands between two statements
	// and drops a heartbeat the hold had earned; the latch cannot.
	worked     bool
	takenAt    time.Time
	execFrom   time.Time
	execTotal  time.Duration
	idleFrom   time.Time
	idleMax    time.Duration
	heartbeats int
	failures   int
	stop       chan struct{}
	stopped    chan struct{}
}

// newLockHold builds the hold for one engine open of the store at doltRootDir,
// inside the store's long phase (phase). The observer it reports to is read
// from the opener's context now, since the hold ends in a close that carries
// none.
func newLockHold(ctx context.Context, stderr io.Writer, doltRootDir string, access engineAccess, phase *longPhase) *lockHold {
	observer := lockObserverFrom(ctx)
	storageDir, lockPath := workspaceStorageDir(doltRootDir), DoltJournalLockPath(doltRootDir)
	privatePath, recordPath := lockHolderRecordNames(lockHolderDir(storageDir, lockPath))
	return &lockHold{
		stderr:      stderr,
		storageDir:  storageDir,
		lockPath:    lockPath,
		privatePath: privatePath,
		recordPath:  recordPath,
		access:      access,
		phase:       phase,
		// The same holdWait every store lock waits with, keyed by Dolt's LOCK,
		// so an engine open and a LockDoltJournalExclusive contender give the
		// same holder the same wait and read the same progress. One per open:
		// backoff state is per-open, and the connector Resets it before each.
		// [LAW:one-source-of-truth]
		wait:   newHoldWait(stderr, storageDir, lockPath, coResidentHolderWaitNow, time.Now),
		now:    time.Now,
		held:   observer.Held,
		waited: observer.Waited,
	}
}

// name is the hold's record name: its arrival, and the stem of its heartbeats.
func (h *lockHold) name() string { return filepath.Base(h.recordPath) }

// record publishes this process as LOCK's holder under the hold's own name,
// once the open that took LOCK has returned, and returns the release that
// retires the record. It wraps a no-op lock release: the hold itself ends
// when the engine closes. A read engine's hold takes no LOCK, so it records
// nothing and its release is a no-op. [LAW:single-enforcer] the one place
// that decides whether an open is recorded.
func (h *lockHold) record(ctx context.Context) func() error {
	if !h.access.holdsLock() {
		return func() error { return nil }
	}
	return recordLockHolder(ctx, h.stderr, h.storageDir, h.lockPath, h.privatePath, h.recordPath, func() error { return nil })
}

// under is ctx carrying this hold as Dolt's journal lock observer: the
// context Dolt must take LOCK under for the hold to learn of it. Every
// engine open runs under it (executingConnector.Connect); a statement that
// creates a database, and so takes that database's LOCK, is run under it by
// its caller.
func (h *lockHold) under(ctx context.Context) context.Context {
	return nbs.WithJournalLockObserver(ctx, nbs.JournalLockObserver{Taken: h.taken, Released: h.end})
}

// taken is Dolt's word that LOCK is held. The hold announces its arrival at
// this instant (not after the open returns, which under load is seconds
// later) and starts its heartbeat. The announce runs inside Dolt's open, so
// it lands before Dolt can let LOCK go: an arrival written after the release
// could overwrite the next holder's. [LAW:no-ambient-temporal-coupling]
//
// A hold has one life: every context it is installed on opens exactly one
// journaled store, which takes LOCK once and lets it go at the engine's
// close. A second Taken would be a LOCK this hold neither heartbeats nor
// reports, so it is said aloud rather than dropped. [LAW:no-silent-failure]
func (h *lockHold) taken() {
	h.mu.Lock()
	if h.state != holdUnheld {
		h.mu.Unlock()
		fmt.Fprintf(h.stderr, "lit: Dolt took %s again under a hold that already took it; this hold is neither heartbeating for nor reporting the second\n", h.lockPath)
		return
	}
	now := h.now()
	h.state, h.takenAt = holdHeld, now
	if h.executing > 0 {
		h.execFrom = now
	} else {
		h.idleFrom = now
	}
	h.stop, h.stopped = make(chan struct{}), make(chan struct{})
	stop, stopped := h.stop, h.stopped
	h.mu.Unlock()
	// The holder directory may not exist yet on a store's first open: the
	// holder record makes it, but only after the open returns. A failure
	// here surfaces as the announce's own failure just below.
	_ = os.MkdirAll(lockHolderDir(h.storageDir, h.lockPath), 0o755)
	h.announce(h.name())
	go h.beat(stop, stopped)
}

// beat runs the heartbeat ticker for one hold until stop closes. The tick's
// decision is tick's, on the instant it is handed; this loop only supplies
// instants. [LAW:effects-at-boundaries]
func (h *lockHold) beat(stop <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)
	ticker := time.NewTicker(holdHeartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			h.tick()
		}
	}
}

// tick writes one heartbeat when the hold is routine, holds LOCK, is not
// inside its store's long phase, and its engine worked since the last tick. So the
// first tick after engine work ends still writes one, for that work: a holder
// that turns to a wait outside its engine sends at most one heartbeat into
// it, and its waiters' clocks start at most one holdHeartbeatInterval late.
func (h *lockHold) tick() {
	h.mu.Lock()
	worked := h.worked || h.executing > 0
	h.worked = false
	if h.state != holdHeld || !h.access.routine() || h.phase.active() || !worked {
		h.mu.Unlock()
		return
	}
	h.sequence++
	name := heartbeatName(h.name(), h.sequence)
	h.mu.Unlock()
	h.announce(name)
}

// announce writes name to LOCK's arrival file, outside mu, and counts it
// under mu. A failed write is counted and reported once per hold; it never
// fails the hold, whose lock is held either way. [LAW:no-silent-failure]
func (h *lockHold) announce(name string) {
	err := announceArrival(lockHolderDir(h.storageDir, h.lockPath), name)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		if h.failures == 0 {
			fmt.Fprintf(h.stderr, "lit: could not write this process's heartbeat for %s (the lock is held; a contender waiting now may give up on a holder still working): %v\n", h.lockPath, err)
		}
		h.failures++
		return
	}
	h.heartbeats++
}

// enter and exit bracket one piece of engine work done for the hold. Work
// before Dolt takes LOCK counts too, so an open's work is already running at
// the instant LOCK is taken.
func (h *lockHold) enter() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if h.executing == 0 && h.state == holdHeld {
		h.idleMax = max(h.idleMax, now.Sub(h.idleFrom))
		h.execFrom = now
	}
	h.executing++
	h.worked = true
}

func (h *lockHold) exit() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.executing--
	h.worked = true
	if h.executing == 0 && h.state == holdHeld {
		now := h.now()
		h.execTotal += now.Sub(h.execFrom)
		h.idleFrom = now
	}
}

// longPhase is a store's long phase inside a routine hold (a migration that
// changes the schema): its engine works, but not as routine work, so no hold
// of the store heartbeats while the phase lasts. The Store owns it and hands
// it to every hold it opens, so an engine rotated mid-phase (a GC-contention
// reconnect inside a migration's commit) opens into the phase rather than out
// of it. [LAW:one-source-of-truth] one fact about the store, not one copy per
// engine.
type longPhase struct{ depth atomic.Int32 }

// enter begins the phase; the returned leave ends it.
func (p *longPhase) enter() (leave func()) {
	p.depth.Add(1)
	return func() { p.depth.Add(-1) }
}

func (p *longPhase) active() bool { return p.depth.Load() > 0 }

// end is the hold's last act: Dolt's word that LOCK was let go, the opener's
// after a failed open, or the close's after the engine closed, whichever
// comes first. Later calls do nothing. A hold that never took LOCK reports
// nothing, since it held nothing.
func (h *lockHold) end() {
	h.mu.Lock()
	if h.state == holdEnded {
		h.mu.Unlock()
		return
	}
	wasHeld := h.state == holdHeld
	h.state = holdEnded
	now := h.now()
	report := LockHold{Lock: filepath.Base(h.lockPath), Routine: h.access.routine()}
	if wasHeld {
		if h.executing > 0 {
			h.execTotal += now.Sub(h.execFrom)
		} else {
			h.idleMax = max(h.idleMax, now.Sub(h.idleFrom))
		}
		report.Held, report.Executing, report.LongestIdle = now.Sub(h.takenAt), h.execTotal, h.idleMax
	}
	stop, stopped := h.stop, h.stopped
	h.mu.Unlock()
	if stop != nil {
		close(stop)
		<-stopped
	}
	if !wasHeld {
		return
	}
	// Counted after the ticker stopped, so a write it had begun is in them.
	h.mu.Lock()
	report.Heartbeats, report.HeartbeatFailures = h.heartbeats, h.failures
	h.mu.Unlock()
	h.held(report)
}

// reportWait tells the observer how the open's wait for LOCK ended, given
// what the open returned.
func (h *lockHold) reportWait(ctx context.Context, openErr error) {
	if wait, waited := h.wait.report(ctx, openErr); waited {
		h.waited(wait)
	}
}

// executingConnector is the one decorator that tells a lockHold when its
// engine works: the connector the store's pool is built on, and every conn,
// statement, row set and transaction it hands out. Every Store statement
// goes through this pool (one connection, openDoltPool), so no engine work
// can bypass it. [LAW:single-enforcer]
//
// Each type below forwards exactly the driver interfaces the embedded
// driver's own types implement, so database/sql takes the same path through
// the wrapper as it took without it.
type executingConnector struct {
	inner *embedded.Connector
	hold  *lockHold
}

var (
	_ driver.Connector = executingConnector{}
	_ io.Closer        = executingConnector{}
)

// within runs one piece of engine work for the hold, and withinErr one that
// returns only an error.
func within[T any](h *lockHold, work func() (T, error)) (T, error) {
	h.enter()
	defer h.exit()
	return work()
}

func withinErr(h *lockHold, work func() error) error {
	h.enter()
	defer h.exit()
	return work()
}

// Connect is where the engine opens, eagerly at a ping or lazily at a
// statement, so it runs under the hold: no open of this pool can take LOCK
// without the hold learning of it. [LAW:single-enforcer]
func (c executingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := within(c.hold, func() (driver.Conn, error) { return c.inner.Connect(c.hold.under(ctx)) })
	if err != nil {
		return nil, err
	}
	return executingConn{inner: conn, hold: c.hold}, nil
}

func (c executingConnector) Driver() driver.Driver { return c.inner.Driver() }

// Close closes the engine, which is the end of a write hold's work: under
// load the engine close was the longest silent stretch of a hold.
func (c executingConnector) Close() error {
	return withinErr(c.hold, c.inner.Close)
}

type executingConn struct {
	inner driver.Conn
	hold  *lockHold
}

var (
	_ driver.Conn        = executingConn{}
	_ driver.ConnBeginTx = executingConn{}
)

func (c executingConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := within(c.hold, func() (driver.Stmt, error) { return c.inner.Prepare(query) })
	if err != nil {
		return nil, err
	}
	return executingStmt{inner: stmt, hold: c.hold}, nil
}

func (c executingConn) Close() error {
	return withinErr(c.hold, c.inner.Close)
}

func (c executingConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c executingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := within(c.hold, func() (driver.Tx, error) { return c.inner.(driver.ConnBeginTx).BeginTx(ctx, opts) })
	if err != nil {
		return nil, err
	}
	c.hold.enter()
	return executingTx{inner: tx, hold: c.hold}, nil
}

// executingStmt forwards the context forms the driver's statements
// implement (single and multi-statement alike).
type executingStmt struct {
	inner driver.Stmt
	hold  *lockHold
}

var (
	_ driver.Stmt             = executingStmt{}
	_ driver.StmtExecContext  = executingStmt{}
	_ driver.StmtQueryContext = executingStmt{}
)

func (s executingStmt) Close() error {
	return withinErr(s.hold, s.inner.Close)
}

func (s executingStmt) NumInput() int { return s.inner.NumInput() }

func (s executingStmt) Exec(args []driver.Value) (driver.Result, error) {
	return within(s.hold, func() (driver.Result, error) { return s.inner.Exec(args) }) //nolint:staticcheck // forwarded as the driver implements it
}

func (s executingStmt) Query(args []driver.Value) (driver.Rows, error) {
	rows, err := within(s.hold, func() (driver.Rows, error) { return s.inner.Query(args) }) //nolint:staticcheck // forwarded as the driver implements it
	return wrapRows(rows, err, s.hold)
}

func (s executingStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return within(s.hold, func() (driver.Result, error) { return s.inner.(driver.StmtExecContext).ExecContext(ctx, args) })
}

func (s executingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := within(s.hold, func() (driver.Rows, error) { return s.inner.(driver.StmtQueryContext).QueryContext(ctx, args) })
	return wrapRows(rows, err, s.hold)
}

// wrapRows wraps a row set as the kind the driver returned: a multi-statement
// row set keeps its result-set iteration, a single one does not gain it.
func wrapRows(rows driver.Rows, err error, hold *lockHold) (driver.Rows, error) {
	if err != nil {
		return nil, err
	}
	single := executingRows{inner: rows, hold: hold}
	if multi, ok := rows.(driver.RowsNextResultSet); ok {
		return executingMultiRows{executingRows: single, inner: multi}, nil
	}
	return single, nil
}

type executingRows struct {
	inner driver.Rows
	hold  *lockHold
}

var _ driver.Rows = executingRows{}

func (r executingRows) Columns() []string { return r.inner.Columns() }

func (r executingRows) Close() error {
	return withinErr(r.hold, r.inner.Close)
}

func (r executingRows) Next(dest []driver.Value) error {
	return withinErr(r.hold, func() error { return r.inner.Next(dest) })
}

type executingMultiRows struct {
	executingRows
	inner driver.RowsNextResultSet
}

var _ driver.RowsNextResultSet = executingMultiRows{}

func (r executingMultiRows) HasNextResultSet() bool { return r.inner.HasNextResultSet() }

func (r executingMultiRows) NextResultSet() error {
	return withinErr(r.hold, r.inner.NextResultSet)
}

// executingTx is engine work from BeginTx to its Commit or Rollback, not
// only while a statement runs: a mutation's own computation between its
// statements (rank smoothing measured 1.9s under load) is the hold's work,
// and nothing that waits on the world runs inside one, since the commit lock
// is taken before the transaction begins and a prompt never runs inside a
// store mutation. database/sql hands exactly one of Commit and Rollback to
// the driver, so the bracket BeginTx opened closes once.
type executingTx struct {
	inner driver.Tx
	hold  *lockHold
}

var _ driver.Tx = executingTx{}

func (t executingTx) Commit() error {
	defer t.hold.exit()
	return withinErr(t.hold, t.inner.Commit)
}

func (t executingTx) Rollback() error {
	defer t.hold.exit()
	return withinErr(t.hold, t.inner.Rollback)
}
