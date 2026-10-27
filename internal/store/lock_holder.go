package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/cenkalti/backoff/v4"
	"github.com/promptctl/primitives/filelock"
)

// This file answers the one question an flock cannot: WHO is holding it. The
// kernel decides whether a lock is held — that authority is not shared, and
// nothing here is ever consulted to make that decision. A holder record only
// puts a name to a holder the kernel has ALREADY reported, so a contender can
// say "pid 7232, `lit backlog`, 29h" instead of "workspace busy".
//
// [LAW:one-source-of-truth] Two facts, two homes, per the lock discipline in
// this package's doc: the flock carries "is this held", a plain record file
// carries "who is holding it". The same split the mirror-pending marker makes
// against the mirror beacon. A record is never evidence of holding — a record
// with no live hold behind it is stale by definition, and readLockHolders
// proves liveness the one way this package permits, by acquiring.
//
// One record per ACQUISITION, not per process or per lock: a uniquely-named
// file, minted under the lock's directory in lockHolderDir, whose creator
// holds it SHARED for exactly as long as it holds the lock. Shared is what
// makes it readable: a reader on Windows, where LockFileEx is mandatory, can
// read a shared-held record but not an exclusively-held one.
//
// [LAW:no-ambient-temporal-coupling] A record is created, held, and filled in
// under a private name, and reaches the name a reader sweeps by being LINKED
// there — so under that name it is complete and held from the instant it
// exists, and there is no moment a reader can catch it half-made. That is what
// makes a reader's cleanup safe: proving a swept record unheld proves its
// holder gone, because no publisher is ever working on a name a reader can
// see. lockHolderPrivatePrefix carries the full account of why the naive
// single-name version cannot be made safe by ordering alone.
//
// NO WAIT EDGE. Nothing here ever waits on a record's flock at all. A reader's
// liveness probe passes maxAttempts 1, and a publisher's own hold is on a name
// nothing else can reach, so it is uncontended by construction. This file
// therefore takes no slot in the package's acquisition order, for the same
// reason the sync-push lock needs none.
//
// One decision does read the registry: how long a contender keeps trying
// (holdWait). It reads only holder names — the records standing and the
// lock's latest arrival — never what any record says, and it decides only
// when the contender stops — the kernel still decides, alone, whether the
// lock is held. A wrong or missing name costs a wait that ends earlier or later than it should, never a
// lock taken or refused wrongly, which keeps the registry descriptive in the
// sense that matters.

// lockHolderDir is the directory of holder records for one lock, under the
// workspace storage dir the caller names — the same rotation-surviving
// position every lit-minted lock file sits in, and never beside the lock
// itself. That difference is the whole reason the root is a parameter: Dolt's
// journal LOCK lives inside the dolt directory because it is Dolt's file, and
// minting lit's records there would put lit state in a tree lit does not own
// and copy it into every `lit snapshots new` artifact. Records are lit's, so
// they live where lit's things live, keyed by the lock's own file name.
//
// [LAW:one-source-of-truth] One naming convention; both the writer and the
// reader take the path from here.
func lockHolderDir(storageDir, lockPath string) string {
	return filepath.Join(storageDir, ".links-lock-holders", filepath.Base(lockPath))
}

// lockHolderRecordPrefix marks a file as a holder record, so a reader can tell
// one from anything else that ever lands in the directory.
const lockHolderRecordPrefix = "holder-"

// lockHolderPrivatePrefix names the record under the name only its own holder
// ever touches. It deliberately does NOT carry lockHolderRecordPrefix, so a
// reader's sweep never sees it — and that, with the link into the swept name,
// is what keeps a reader's cleanup from destroying a live record.
//
// A reader retires any record it proves unheld, deciding that under the
// record's own hold but acting on it after dropping that hold, so the proof
// outlives what established it. Worse, the probe that establishes it OPENS
// WITH O_CREATE, so a reader can manufacture the very record it then proves
// unheld. Let readers near a name a publisher is still working on and both
// bite: one reader unlinks the name, a second recreates it empty, and the
// publisher fills and publishes an inode nothing holds — a holder silently
// unnamed for the life of its lock, which is the one failure this whole file
// exists to prevent.
//
// Splitting the names retires the question rather than narrowing the window. A
// record reaches lockHolderRecordPrefix by being LINKED there, already held
// and already filled, so every name a reader can see is a finished record and
// proving one unheld really does prove its holder dead. Every name a publisher
// works on is one no reader will ever open.
//
// Both names live exactly as long as the record does: one inode, two
// directory entries, retired together by whoever ends it — its own holder at
// release, or the sweep that proves it dead (readLockHolder retires the
// private sibling on the strength of the same proof, since it is the same
// inode). The one case the split gives up is a publisher killed before it
// links, which leaves a private name no record name points at and so nothing
// collects. A stray file nothing ever opens is the cheap end of this trade —
// and nothing does open it, because no later process can draw its name
// (lockHolderProcessNonce) — while the holder it would otherwise cost is the
// expensive one.
const lockHolderPrivatePrefix = "private-"

// lockHolderProcessNonce and lockHolderRecordSeq, with the pid, make a
// record's names unique among every record ever minted, not only among those
// live at once. The pid alone is not enough: the kernel reuses it, and the
// sequence restarts in every process, so a pid that once died holding a
// record would draw that record's name again. The nonce is what a reused pid
// does not repeat. Both names are still created by primitives that refuse to
// replace (O_EXCL, and link), so were a name ever drawn twice the publish
// would fail loudly rather than overwrite.
//
// [LAW:no-shared-mutable-globals] The nonce is drawn once and never written
// again. The sequence has one writer, one operation, one invariant: every
// read is an Add, and the value means nothing except that it differs from
// the last.
var (
	lockHolderProcessNonce = func() string {
		var nonce [4]byte
		// crypto/rand.Read never returns an error; it crashes the program on
		// the one platform failure it cannot recover from.
		_, _ = rand.Read(nonce[:])
		return hex.EncodeToString(nonce[:])
	}()
	lockHolderRecordSeq atomic.Uint64
)

// lockHolderRecordNames draws the two names this process's next acquisition
// lives under: the private one only its own holder ever touches, and the one
// a reader sweeps.
func lockHolderRecordNames(dir string) (privatePath, recordPath string) {
	return lockHolderRecordNamesFor(dir, os.Getpid(), lockHolderProcessNonce, lockHolderRecordSeq.Add(1))
}

// lockHolderRecordNamesFor cuts both names of the record a process with this
// pid and nonce mints as its seq'th, whatever process that is.
// [LAW:one-source-of-truth] Both are cut from a single suffix here, which is
// what lets a sweep read the split backwards to find a dead record's private
// sibling; privateNameFor is that inverse.
func lockHolderRecordNamesFor(dir string, pid int, nonce string, seq uint64) (privatePath, recordPath string) {
	suffix := fmt.Sprintf("%d-%s-%d", pid, nonce, seq)
	return filepath.Join(dir, lockHolderPrivatePrefix+suffix),
		filepath.Join(dir, lockHolderRecordPrefix+suffix)
}

// heartbeatName is the seq'th heartbeat of the hold whose record is named
// recordName, and isHeartbeat tells one from a record name: a record name has
// no '.', so the one character marks the shape both read.
// [LAW:one-source-of-truth] the shape is written and read here only.
func heartbeatName(recordName string, seq uint64) string {
	return fmt.Sprintf("%s.%d", recordName, seq)
}

func isHeartbeat(name string) bool { return strings.Contains(name, ".") }

// privateNameFor names the private sibling of the record at recordPath. The
// two names are one inode, so proving the swept name unheld proves the private
// one unheld by the same acquisition — and retiring only the swept name would
// strand its sibling with nothing left that could collect it.
func privateNameFor(recordPath string) string {
	dir, name := filepath.Split(recordPath)
	return filepath.Join(dir, lockHolderPrivatePrefix+strings.TrimPrefix(name, lockHolderRecordPrefix))
}

// lockHolderRecord is what one holder says about itself. It is descriptive
// only: no lock decision reads a record's contents (a publisher reading back
// its own decides only whether that record goes out), so a record that is
// somehow wrong degrades a diagnostic message and can do nothing else.
type lockHolderRecord struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	Since   time.Time `json:"since"`
}

// recordLockHolder publishes this process's claim on lockPath under the
// record names the acquisition drew (lockHolderRecordNames), and returns a
// release that retires it. It announces nothing: the acquisition announced
// its arrival when it took the lock, under the same name. The returned release wraps lockRelease — the hold
// on the lock itself — so a caller has exactly one thing to defer and cannot
// retire the record while keeping the lock, or the reverse.
//
// A recording failure is loud but never fatal: refusing the lock because a
// DIAGNOSTIC could not be written would turn a working workspace into a broken
// one over a message nobody had asked for. Same demotion, and the same reason,
// as SettleCommitLockRelease's post-success release failure.
// [LAW:no-silent-failure] loud, but not a false failure.
//
// Every publish first retires the records of holders that are gone. A record
// whose holder never ran its release — one SIGKILLed, or one that leaves its
// hold to process exit by design, as a mirror-beacon claimant does — has
// nobody else to collect it: a contender reads the directory only when it
// has waited long enough to report, which an uncontended lock never does. The
// publisher is the one reader every acquisition has, so no record outlives
// its holder past the next acquisition. (A private name stranded by a
// publisher killed before it links is the exception lockHolderPrivatePrefix
// accounts for: no sweep may touch a private name.)
// [LAW:dataflow-not-control-flow] The sweep runs on every acquisition; an
// empty directory is simply a sweep with nothing to retire.
//
// That sweep is also what demotes a failed retirement at release, the way a
// failed publish is demoted: a record this release could not remove has lost
// its hold, so the next acquisition proves it unheld and retires it. On
// Windows that failure needs nothing worse than a concurrent sweep's probe
// holding the file open, since filelock's open denies the delete right.
func recordLockHolder(ctx context.Context, stderr io.Writer, storageDir, lockPath, privatePath, recordPath string, lockRelease func() error) func() error {
	if err := retireDeadLockHolders(storageDir, lockPath); err != nil {
		fmt.Fprintf(stderr, "lit: could not retire the records of holders gone from %s (the lock is held; every acquisition retries, so this repeats until they can be retired): %v\n", lockPath, err)
	}
	releaseRecord, err := publishLockHolder(ctx, storageDir, lockPath, privatePath, recordPath)
	if err != nil {
		fmt.Fprintf(stderr, "lit: could not record this process as the holder of %s (the lock is held; only the diagnostic naming this holder is missing): %v\n", lockPath, err)
		return lockRelease
	}
	return func() error {
		// Retire the record BEFORE dropping the lock, so no instant exists in
		// which the next acquirer holds the lock while this record still
		// answers for it.
		if err := releaseRecord(); err != nil {
			fmt.Fprintf(stderr, "lit: could not retire this process's holder record for %s (the lock is still released; a later acquisition retires the record): %v\n", lockPath, err)
		}
		return lockRelease()
	}
}

// publishLockHolder publishes this process's claim on lockPath: what this
// process would want said about it, handed to the protocol that says it.
func publishLockHolder(ctx context.Context, storageDir, lockPath, privatePath, recordPath string) (func() error, error) {
	dir := lockHolderDir(storageDir, lockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ensure holder dir: %w", err)
	}
	payload, err := json.Marshal(lockHolderRecord{
		PID:     os.Getpid(),
		Command: holderCommand(ctx),
		Since:   time.Now(),
	})
	if err != nil {
		return nil, fmt.Errorf("encode holder record: %w", err)
	}
	return mintHeldRecord(privatePath, recordPath, payload)
}

// lockHolderArrivalName is the file in a lock's holder directory that names
// the lock's latest sign of progress: the record name of the most recent
// acquisition, or a working routine holder's heartbeat, `<name>.<sequence>`
// (lockHold). Either is a name no earlier writing used, which is all a waiter
// reads from it. It carries neither record prefix, so no sweep or listing
// ever reads it as a record.
//
// It exists because a record lives exactly as long as its hold, and a hold can
// begin and end between two of a contender's polls: a `lit new` holds Dolt's
// LOCK for ~60ms against a 100ms poll, so a contender reading which records
// exist misses most of a queue's arrivals. This file outlives the record that
// announced it, so an arrival stays visible until the next one replaces it.
// [FRAMING:representation] the contender's map of "the queue moved" is redrawn
// on every acquisition, not sampled from a territory that changes faster than
// it is read.
const lockHolderArrivalName = "arrival"

// announceArrival names recordName as the lock's latest arrival, overwriting
// the last one in place. Record names are unique among every record ever
// minted, and a heartbeat is one with a sequence after it, so the file's
// content changes on every acquisition and every heartbeat and never returns
// to an earlier value.
//
// In place, with no temporary name and no rename: a reader can catch the file
// torn, or — under two shared holders writing at once — spliced from both
// names, but only while acquisitions are writing it. A reading no acquisition
// wrote can therefore count real arrivals extra times, each restarting a
// contender's clock a poll later than the arrivals alone would, but it never
// reports a queue moving while nothing arrives.
//
// Never empty: the name is padded to lockArrivalWidth and written over the
// whole file at offset 0, with no truncation, so a reader sees the last name
// until the next one lands. A truncating write leaves the file empty until
// its data lands, and under a storm of waiters polling this one file that
// window measured up to 1.7s per write: a heartbeat every waiter read as
// nothing.
//
// A rename would add a private name to strand and, on Windows, fail against
// any reader holding the file open.
// [LAW:no-ambient-temporal-coupling] the announcement is a value an
// acquisition writes, not an interval a reader has to catch.
func announceArrival(dir, recordName string) error {
	if len(recordName) > lockArrivalWidth {
		return fmt.Errorf("announce arrival: name %q is longer than the arrival file's %d bytes", recordName, lockArrivalWidth)
	}
	f, err := os.OpenFile(filepath.Join(dir, lockHolderArrivalName), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := f.WriteAt([]byte(recordName+strings.Repeat(" ", lockArrivalWidth-len(recordName))), 0)
	return errors.Join(writeErr, f.Close())
}

// lockArrivalWidth is the arrival file's size: every name is padded to it, so
// each write covers the whole file. A heartbeat name is under 80 bytes
// (holder-<pid>-<nonce>-<seq>.<sequence>).
const lockArrivalWidth = 128

// lockArrival is one reading of a lock's latest arrival: the record name that
// announced it and when it was written. The zero value is a lock no
// acquisition has announced.
type lockArrival struct {
	name string
	at   time.Time
}

// latestArrival reads the lock's latest arrival. A lock nothing has announced
// yet is the zero lockArrival, not an error: every lock starts that way, and
// under a holder that announces nothing (a foreign dolt, a lit older than
// arrivals) it stays that way, so the wait reduces to its plain elapsed
// budget. Any other failure is returned, because a waiter that cannot read
// arrivals is back to the blindness they exist to end, and someone has to
// hear why. [LAW:no-silent-failure]
func latestArrival(storageDir, lockPath string) (lockArrival, error) {
	f, err := os.Open(filepath.Join(lockHolderDir(storageDir, lockPath), lockHolderArrivalName))
	if errors.Is(err, os.ErrNotExist) {
		return lockArrival{}, nil
	}
	if err != nil {
		return lockArrival{}, fmt.Errorf("read lock arrival: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return lockArrival{}, fmt.Errorf("read lock arrival: %w", err)
	}
	name, err := io.ReadAll(f)
	if err != nil {
		return lockArrival{}, fmt.Errorf("read lock arrival: %w", err)
	}
	return lockArrival{name: strings.TrimRight(string(name), " "), at: info.ModTime()}, nil
}

// describeArrival is the holder account's clause about the lock's latest
// arrival: how long ago the last lit acquisition announced itself, or the
// holder's last heartbeat was written, told apart by the name's shape
// (isHeartbeat). It says no
// more than that — a holder that announces nothing, or a shared holder
// joining after it, can stand behind the age. Beside a refusal it tells apart
// the two ways a wait ends — a queue that stopped moving a whole wait ago, or
// a waiter that could not see a queue still moving — and it is where an
// unreadable arrival surfaces, in every notice and every refusal.
// The age is read from the file's modification time, for the account alone:
// no wait decision reads it.
func describeArrival(storageDir, lockPath string) string {
	latest, err := latestArrival(storageDir, lockPath)
	switch {
	case err != nil:
		return fmt.Sprintf("arrivals unreadable, so a waiter sees only the holders standing at each poll: %v", err)
	case latest.at.IsZero():
		return "no lit acquisition has announced an arrival here"
	case isHeartbeat(latest.name):
		return fmt.Sprintf("the holder's last heartbeat was %s ago", time.Since(latest.at).Round(time.Millisecond))
	default:
		return fmt.Sprintf("the last lit acquisition announced itself %s ago", time.Since(latest.at).Round(time.Millisecond))
	}
}

type holderCommandKey struct{}

// WithHolderCommand is ctx carrying the command line of the run it belongs
// to, which every lock taken under it names in its holder record. The command,
// not the pid, is what tells one wedged lit from another (`backlog` vs
// `snapshots new`), and it comes from the run rather than the process because
// one process can hold several runs, each its own command.
// [LAW:no-shared-mutable-globals]
func WithHolderCommand(ctx context.Context, command string) context.Context {
	return context.WithValue(ctx, holderCommandKey{}, command)
}

// holderCommand is the command ctx carries, or "" for a lock taken outside
// any run that named one: a test or tool opening a store directly.
func holderCommand(ctx context.Context) string {
	command, _ := ctx.Value(holderCommandKey{}).(string)
	return command
}

// mintHeldRecord runs the four steps that make a holder record: create it,
// fill it in, take the shared hold that makes it live — refusing it if the held
// file no longer says what was filled in — and only then give it the name a
// reader sweeps. The order is the point — under that name the record
// is complete and held from the instant it exists, so no reader ever meets a
// half-made one.
//
// Filled before held, because the hold forbids the fill: filelock's Windows
// LockFileEx covers the whole file, and a shared byte-range lock denies WRITE
// to every handle, the locker's own included. Filling first exposes nothing —
// no reader ever opens a private name — so the swept name still arrives
// complete and held.
func mintHeldRecord(privatePath, recordPath string, payload []byte) (func() error, error) {
	if err := writeNewRecord(privatePath, payload); err != nil {
		return nil, err
	}
	release, err := holdWrittenRecord(privatePath, payload)
	if err != nil {
		return nil, err
	}
	// A hold rides the inode, not the name, so the record a reader now finds
	// under recordPath is the one this call is already holding — and this is
	// the first instant any reader can see it at all.
	//
	// Linked, never renamed: MoveFileEx opens its source for DELETE, and
	// filelock holds this file through os.OpenFile, whose Windows share mode
	// omits FILE_SHARE_DELETE — so a rename here fails with a sharing
	// violation on every acquisition on that platform, where nothing in CI
	// would see it. CreateHardLink asks the source for no such right. Linking
	// also refuses to replace an existing name, which is what keeps a record
	// some other holder is still alive under from being published over.
	if err := os.Link(privatePath, recordPath); err != nil {
		return nil, errors.Join(fmt.Errorf("publish holder record: %w", err), release(), retireRecord(privatePath))
	}
	// Both names, and only once the hold is dropped: unlinking either would
	// want the same DELETE right the open handle denies on Windows.
	return func() error {
		return errors.Join(release(), retireRecord(recordPath), retireRecord(privatePath))
	}, nil
}

// writeNewRecord creates the record under its private name and writes the
// payload through the creating handle. O_EXCL so a name already on disk is
// refused here rather than adopted as this holder's record.
// lockHolderProcessNonce keeps any name from being drawn twice; this is what
// makes a breach of that loud.
// [LAW:no-silent-failure]
func writeNewRecord(privatePath string, payload []byte) error {
	file, err := os.OpenFile(privatePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create holder record: %w", err)
	}
	_, writeErr := file.Write(payload)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return errors.Join(fmt.Errorf("write holder record: %w", err), retireRecord(privatePath))
	}
	return nil
}

// holdWrittenRecord takes the shared hold on the record writeNewRecord wrote,
// and refuses a hold on a file that does not say what was written, retiring
// whatever file the refused hold leaves under the private name. filelock
// opens with O_CREATE, so a record removed from under its name before the hold
// lands would be recreated empty and held — a file naming nobody that the link
// would then publish as this holder, unnamed for the life of its lock. Reading
// the held name back is what catches it: a shared hold permits reads on every
// platform, and the contents, not the inode, are what a reader is told.
// [LAW:no-silent-failure]
//
// One attempt is the whole budget: no reader will ever open this name, so
// the hold is uncontended by construction and contention here would mean
// the invariant is broken, not that waiting would help.
func holdWrittenRecord(privatePath string, payload []byte) (func() error, error) {
	release, acquired, err := filelock.Acquire(context.Background(), privatePath, false, 1, 0)
	if err != nil || !acquired {
		return nil, errors.Join(fmt.Errorf("hold holder record (acquired=%v): %w", acquired, err), retireRecord(privatePath))
	}
	held, err := os.ReadFile(privatePath)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("read back held holder record: %w", err), release(), retireRecord(privatePath))
	}
	if !bytes.Equal(held, payload) {
		return nil, errors.Join(fmt.Errorf("hold holder record: %s is no longer the record this process wrote", filepath.Base(privatePath)), release(), retireRecord(privatePath))
	}
	return release, nil
}

// retireRecord ensures a record file is gone. Already-gone is the goal
// reached, not a failure: a released holder retires its own record, and a
// sweep retires any record it proves unheld, so a record a holder has just let
// go can be removed by either. Two parties may therefore remove one record,
// and neither is wrong. Every other removal failure still surfaces.
// [LAW:no-silent-failure]
func retireRecord(recordPath string) error {
	if err := os.Remove(recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("retire holder record: %w", err)
	}
	return nil
}

// describeLockHolders renders the one sentence a contender prints about a lock
// it could not take: who holds it, running what, and for how long.
//
// [LAW:parse-dont-validate] Directory entries and JSON bytes become the one
// rendered account callers use; no caller re-derives a holder from lock
// mechanics, and none of them can, because the parse keeps nothing.
//
// Called only after the kernel has already reported contention, so an empty
// account never means "free" — it means the holder left no record: a process
// not built from this lit, or one holding the file for its own reasons.
func describeLockHolders(storageDir, lockPath string) string {
	holders, problems := readLockHolders(storageDir, lockPath)
	described := make([]string, 0, len(holders))
	for _, holder := range holders {
		described = append(described, fmt.Sprintf(
			"pid %d (%s) holding since %s (%s)",
			holder.PID,
			renderHolderCommand(holder.Command),
			holder.Since.Format(time.RFC3339),
			time.Since(holder.Since).Round(time.Second),
		))
	}
	if len(described) == 0 {
		described = append(described, unnamedHolder(problems))
	}
	account := "held by " + strings.Join(described, ", ")
	return strings.Join(append([]string{account, describeArrival(storageDir, lockPath)}, problems...), "; ")
}

// unnamedHolder is what the account says about a holder no record named. After
// a clean read that is a fact: nothing recorded itself. After a failed read it
// is not — the records may name the holder perfectly well and this call could
// not see them — and asserting the guess anyway sends an operator hunting a
// foreign process while the real cause sits in the problem list beside it.
func unnamedHolder(problems []string) string {
	if len(problems) == 0 {
		return "a process that left no record (a foreign holder, or a lit older than holder records)"
	}
	return "a process these records could not name"
}

// readLockHolders returns the live records under lockPath's holder directory,
// plus an account of everything that went wrong reading them, retiring each
// dead record it meets (probeLockHolder).
//
// [LAW:no-silent-failure] Read failures travel back beside the holders instead
// of shrinking the account silently: a diagnostic that quietly reports fewer
// holders than exist is worse than the silence this whole file exists to end.
func readLockHolders(storageDir, lockPath string) ([]lockHolderRecord, []string) {
	dir := lockHolderDir(storageDir, lockPath)
	names, err := holderRecordNames(dir)
	if err != nil {
		return nil, []string{fmt.Sprintf("holder records unreadable: %v", err)}
	}
	var holders []lockHolderRecord
	var problems []string
	for _, name := range names {
		holder, problem := readLockHolder(filepath.Join(dir, name))
		holders = append(holders, holder...)
		problems = append(problems, problem...)
	}
	return holders, problems
}

// retireDeadLockHolders retires every record under lockPath's holder directory
// whose holder is gone, and reads nothing else: the sweep a publisher runs,
// which wants no account, only the directory cleared of the dead.
func retireDeadLockHolders(storageDir, lockPath string) error {
	dir := lockHolderDir(storageDir, lockPath)
	names, err := holderRecordNames(dir)
	if err != nil {
		return err
	}
	errs := make([]error, 0, len(names))
	for _, name := range names {
		_, probeErr := probeLockHolder(filepath.Join(dir, name))
		errs = append(errs, probeErr)
	}
	return errors.Join(errs...)
}

// probeLockHolder decides whether the record at recordPath still has a live
// holder, and retires the record when it does not — both its names, since
// the one proof covers the inode they share.
//
// Liveness is decided the one way this package's discipline permits — by
// acquiring, which is right on every death mode including SIGKILL — never by
// a PID probe or an age threshold. A record whose hold the probe takes had no
// live owner, and that is safe to conclude only because no publisher ever
// works under a name a sweep can see: a record arrives under its swept name
// finished and held, by link (lockHolderPrivatePrefix).
//
// [LAW:single-enforcer] The one place a record is judged dead, for the
// account a contender reads and the sweep a publisher runs alike.
func probeLockHolder(recordPath string) (live bool, err error) {
	// maxAttempts 1 keeps this a probe: it decides liveness and takes custody
	// of nothing, so no process ever waits on a holder record.
	release, acquired, err := filelock.Acquire(context.Background(), recordPath, true, 1, 0)
	if err != nil {
		return false, fmt.Errorf("holder record %s unprobeable: %w", filepath.Base(recordPath), err)
	}
	if !acquired {
		return true, nil
	}
	if err := errors.Join(release(), retireRecord(recordPath), retireRecord(privateNameFor(recordPath))); err != nil {
		return false, fmt.Errorf("retiring dead holder record %s: %w", filepath.Base(recordPath), err)
	}
	return false, nil
}

// readLockHolder resolves one record file into at most one live holder. The
// content is read before liveness is probed: reading is what a shared hold
// permits on every platform, while the exclusive probe that decides liveness
// would, on Windows, lock out the read it is deciding about.
func readLockHolder(recordPath string) ([]lockHolderRecord, []string) {
	payload, readErr := os.ReadFile(recordPath)
	live, err := probeLockHolder(recordPath)
	if err != nil {
		return nil, []string{err.Error()}
	}
	if !live {
		return nil, nil
	}
	if readErr != nil {
		return nil, []string{fmt.Sprintf("a holder is live but its record %s is unreadable: %v", filepath.Base(recordPath), readErr)}
	}
	if len(payload) == 0 {
		// A record is filled in before it is ever named, so an empty one under
		// a live hold was manufactured by a probe rather than published by a
		// holder: filelock opens with O_CREATE, so a reader meeting a name a
		// released holder has already retired creates it, holds it, and is
		// then read by the next reader through. It names nobody, and calling
		// it malformed would report the sweep's own footprint as a problem
		// with somebody's record.
		return nil, nil
	}
	var holder lockHolderRecord
	if err := json.Unmarshal(payload, &holder); err != nil {
		return nil, []string{fmt.Sprintf("a holder is live but its record %s does not parse: %v", filepath.Base(recordPath), err)}
	}
	return []lockHolderRecord{holder}, nil
}

// holderCommandLimit caps a rendered command line. A holder's argv is another
// process's text arriving in an operator's terminal, so it is rendered, never
// echoed: control characters (an embedded newline forging a second lit: line,
// an escape sequence repainting the screen) are replaced and the length is
// bounded.
const holderCommandLimit = 120

func renderHolderCommand(command string) string {
	rendered := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '?'
	}, command)
	// Runes, not bytes: a byte cut lands mid-rune on any multibyte argument
	// and renders the mojibake this function exists to prevent.
	if runes := []rune(rendered); len(runes) > holderCommandLimit {
		return string(runes[:holderCommandLimit]) + "…"
	}
	return rendered
}

// holdWait is the one retry policy every contender for a store lock waits
// with — the lit-minted locks through acquireStoreLock, and Dolt's LOCK
// through the engine connector and the chunk-store open, which take
// backoff.BackOff. It polls every storeLockPollInterval and stops once the
// holders in front of the contender have stood still for the wait: the clock
// starts at Reset, and restarts whenever the lock's latest arrival
// (lockHolderArrivalName) names a holder other than the one it named at the
// last reading, so a queue of short holders — a hundred `lit new`
// serializing at tens of milliseconds each — is waited out however long it
// runs, while a single holder standing for the whole wait ends the
// contender's attempt. "Cannot get the store" is a holder that is not
// moving, not a store that is busy.
//
// Only an ARRIVAL restarts the clock, never a departure, and an arrival is a
// holder name this wait has never seen. A name is minted by an acquisition
// and nothing else, and the wait reads names two ways: the records standing
// at a poll, and the lock's latest arrival (lockHolderArrivalName), which
// outlives the hold it announces, so a holder that came and went between two
// polls is still seen. Either sighting is proof of a new holder, so they feed
// one set rather than competing: a holder whose announcement failed, or a lit
// older than arrivals, still shows by its record, and a brief holder still
// shows by its arrival. A name vanishing is not proof of anything the
// contender cares about — a reader's sweep retires a dead record
// (readLockHolder), and its probe can put an empty one back under the same
// name — and counting that as progress would let a notice tick, or a peer's
// failure, hold a contender past its wait. Names ever seen are kept for the
// life of the wait so the same name cannot arrive twice.
//
// There is no ceiling above the wait. Under holders that keep arriving the
// contender keeps waiting, reporting who it waits on every
// lockWaitNoticeInterval, and leaves on its context: an exclusive contender
// (a snapshot copy) can in principle wait out a write storm that never
// pauses, and it lands when the storm does, where a ceiling would have
// failed it with a holder that was never wedged. Fails-in-seconds is the
// promise about a holder that stands still, under every budget but the
// bootstrap lock's, which is none (acquireBootstrapLock).
//
// A working routine holder does not stand still: its heartbeat (lockHold) is
// a fresh name in the arrival file while its engine works, read here like any
// arrival. So one routine hold is waited out for as long as its engine keeps
// working, however long a loaded machine stretches it. So that wait has no
// bound either: a routine hold wedged inside a Dolt statement
// heartbeats like a slow one, since from outside the two cannot be told apart
// at any finite time, and its contenders wait, naming it every
// lockWaitNoticeInterval. A slow-but-live routine holder failing every writer
// behind it is the failure this exists to end, so a Dolt bug is what gives
// way. A long hold sends no heartbeat and is never waited on that way.
//
// [LAW:dataflow-not-control-flow] Every wait runs the same loop; what varies
// is the lock's directory and its budget, both values. A holder that leaves
// no name (a foreign dolt) contributes no progress, so under
// it the rule is the plain elapsed budget it reduces to when there is nothing
// to read.
//
// [LAW:no-ambient-temporal-coupling] No timestamp is trusted; the signal is
// a name, which only an acquisition mints.
//
// The policy also gives the wait notice. NextBackOff runs only after an
// attempt was refused because something holds the lock, so "a contender is
// waiting" is a fact this policy knows rather than infers, and the notice
// cannot narrate an acquisition that was merely slow — an engine open
// recovering its journal, a holder record written on a loaded disk — as a
// wait on a holder that does not exist. [LAW:one-source-of-truth]
type holdWait struct {
	// stderr is where the wait reports itself once it outlasts the notice
	// grace: the run's operator channel.
	stderr     io.Writer
	storageDir string
	lockPath   string
	wait       func() time.Duration
	now        func() time.Time
	// known is every holder name this wait has seen; nil until its first
	// readable reading, which is the baseline every later one is judged
	// against, so a reading that failed at Reset cannot make a long-standing
	// holder look new at the next poll.
	known map[string]struct{}
	// started is when this wait began; since is when its holders last
	// changed, the stand-still clock the wait is judged on.
	started   time.Time
	since     time.Time
	noticeDue time.Time
	// heldAt is the latest poll that found the lock held since Reset, zero
	// when none has, and progress how many holder names arrived after the
	// baseline: what a LockWait reports, read off the polls the wait took
	// rather than a reading of its own, and the first is what makes it one.
	heldAt   time.Time
	progress int
	// stoodStill is whether this wait gave up because its holders stood still
	// for the whole wait: the policy's own decision, kept so the report reads
	// it rather than inferring it from an error every caller shapes its own
	// way (an engine open hands back Dolt's lock error whether the wait gave up
	// or its context ended).
	stoodStill bool
}

// newHoldWait builds the policy for one lock. wait is read at each poll, not
// at construction, so a policy built into a long-lived connector still waits
// the figure a test has since shrunk. [LAW:one-source-of-truth] now is the
// clock the wait is measured on, and it must be the timeline the retry loop
// sleeps on between polls: acquireStoreLockOn takes both from one lockClock,
// so a test that steps the instants also takes the sleeps; an engine open
// passes time.Now beside Dolt's own real timer. time.Now's readings carry the
// monotonic clock, so a wall-clock step cannot shorten or stretch a wait.
// [LAW:effects-at-boundaries]
func newHoldWait(stderr io.Writer, storageDir, lockPath string, wait func() time.Duration, now func() time.Time) *holdWait {
	return &holdWait{stderr: stderr, storageDir: storageDir, lockPath: lockPath, wait: wait, now: now}
}

// Reset starts the clock and takes the first reading of who is there.
func (w *holdWait) Reset() {
	w.started = w.now()
	w.known, w.since, w.noticeDue = nil, w.started, w.started.Add(lockWaitNoticeGrace)
	w.heldAt, w.progress, w.stoodStill = time.Time{}, 0, false
	w.arrived()
}

// NextBackOff is called after each failed attempt: it restarts the clock when
// a holder has arrived since the last reading, stops the retry when the
// holders in front have stood for the whole wait, and otherwise reports the
// wait once it has outlasted the notice grace and every notice interval after.
// A wait that stops is over, so it says nothing here: the refusal that
// follows names the holders, and "still waiting" beside it would be false.
// One reading of the clock serves the whole decision, so the arrival, the
// notice and the elapsed figure are measured at the same instant.
func (w *holdWait) NextBackOff() time.Duration {
	now := w.now()
	w.heldAt = now
	if w.arrived() {
		w.since = now
	}
	if now.Sub(w.since) >= w.wait() {
		w.stoodStill = true
		return backoff.Stop
	}
	if !now.Before(w.noticeDue) {
		fmt.Fprintf(w.stderr, "lit: still waiting for %s after %s; %s\n",
			w.lockPath, now.Sub(w.started).Round(time.Second), describeLockHolders(w.storageDir, w.lockPath))
		w.noticeDue = now.Add(lockWaitNoticeInterval)
	}
	return storeLockPollInterval
}

// arrived takes one reading of the holder names and reports whether any is
// new to this wait, remembering every name it sees. The first readable
// reading is the baseline and reports nothing.
func (w *holdWait) arrived() bool {
	names, readable := w.holderNames()
	if !readable {
		return false
	}
	baseline := w.known == nil
	if baseline {
		w.known = map[string]struct{}{}
	}
	arrived := false
	for _, name := range names {
		if _, seen := w.known[name]; !seen {
			w.known[name] = struct{}{}
			if !baseline {
				arrived = true
				w.progress++
			}
		}
	}
	return arrived
}

// report is this wait as a LockWait, given what the acquisition it policed
// returned, and whether there was a wait to report: an acquisition that never
// found its lock held waited for nothing. A success is the acquisition's to
// say, since a policy is never told of one; giving up on holders that stood
// still is the policy's own decision; a context that ended cut the wait
// (backoff stops on it before asking the policy); anything else failed the
// attempt. [LAW:one-source-of-truth] each outcome is read from the one place
// that decided it, never from the shape of an error.
func (w *holdWait) report(ctx context.Context, acquireErr error) (LockWait, bool) {
	if w.heldAt.IsZero() {
		return LockWait{}, false
	}
	outcome := LockWaitFailed
	switch {
	case acquireErr == nil:
		outcome = LockWaitAcquired
	case w.stoodStill:
		outcome = LockWaitStoodStill
	case ctx.Err() != nil:
		outcome = LockWaitCancelled
	}
	return LockWait{
		Lock:         filepath.Base(w.lockPath),
		Waited:       w.heldAt.Sub(w.started),
		ProgressSeen: w.progress,
		Outcome:      outcome,
	}, true
}

// holderNames is every holder name the lock shows at one instant: its standing
// records and its latest arrival. It is unreadable only when both are; either
// failure alone surfaces in the holder account (readLockHolders,
// describeArrival), which every notice and every refusal renders.
func (w *holdWait) holderNames() ([]string, bool) {
	names, listErr := holderRecordNames(lockHolderDir(w.storageDir, w.lockPath))
	latest, arrivalErr := latestArrival(w.storageDir, w.lockPath)
	if latest.name != "" {
		names = append(names, latest.name)
	}
	return names, listErr == nil || arrivalErr == nil
}

// holderRecordNames lists the record names under a lock's holder directory —
// the swept names only, never a private one. Nothing is opened or probed;
// liveness is probeLockHolder's business. A directory that does not exist yet
// has no holders.
// [LAW:one-source-of-truth] The one listing the account, the sweep and the
// wait all read.
//
// The kind is checked before the read because a directory read cannot be
// trusted with it: on Windows it reads a regular file as an empty directory
// (os/dir_windows.go takes the ERROR_FILE_NOT_FOUND its listing call returns
// on a file handle for "no more entries"), so a file squatting on the holder
// dir would read as no holders rather than as the unreadable records it is.
// One handle answers both, so the kind checked is the kind listed.
func holderRecordNames(dir string) (names []string, err error) {
	listing, err := os.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, listing.Close()) }()
	info, err := listing.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	entries, err := listing.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), lockHolderRecordPrefix) {
			names = append(names, entry.Name())
		}
	}
	// Sorted, as os.ReadDir sorts: the account renders holders in this order.
	slices.Sort(names)
	return names, nil
}

var (
	// lockWaitNoticeGrace is how long a wait stays quiet before it is worth
	// reporting: long enough that the ordinary case — two lit commands
	// overlapping for a few milliseconds — prints nothing, short enough to
	// land inside coResidentHolderWait, the stand-still budget of every wait
	// that gives up, so a contender that is about to fail has
	// already said who it is waiting on. A grace under a second would report
	// the overlaps this one exists to ignore. lockWaitNoticeInterval then
	// repeats the notice, because a wait behind a queue of holders can run
	// as long as the queue does, and a wait that stops reporting itself is
	// indistinguishable from the hang this notice exists to rule out.
	//
	// Package variables, not constants, by the convention
	// coResidentHolderWait already sets:
	// tests whose premise is a wait must shrink the budget rather than sleep
	// through the production one.
	lockWaitNoticeGrace    = 2 * time.Second
	lockWaitNoticeInterval = 30 * time.Second
)
