package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

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
// under a private name, and reaches the name a reader sweeps by being renamed
// there — so under that name it is complete and held from the instant it
// exists, and there is no moment a reader can catch it half-made. That is what
// makes a reader's cleanup safe: proving a swept record unheld proves its
// holder gone, because no publisher is ever working on a name a reader can
// see. lockHolderMintingPrefix carries the full account of why the naive
// single-name version cannot be made safe by ordering alone.
//
// NO WAIT EDGE. Nothing here ever waits on a record's flock at all. A reader's
// liveness probe passes maxAttempts 1, and a publisher's own hold is on a name
// nothing else can reach, so it is uncontended by construction. This file
// therefore takes no slot in the package's acquisition order, for the same
// reason the sync-push lock needs none.

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

// lockHolderMintingPrefix names a record that is not one yet: created, held,
// and being filled in. It deliberately does NOT carry lockHolderRecordPrefix,
// so a reader's sweep never sees it — and that, with the rename into the final
// name, is what keeps a reader's cleanup from destroying a live record.
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
// record reaches lockHolderRecordPrefix by being RENAMED there, already held
// and already filled, so every name a reader can see is a finished record and
// proving one unheld really does prove its holder dead. Every name a publisher
// works on is one no reader will ever open.
//
// The cost is the one case the split gives up: a publisher killed between
// creating its mint and renaming it — a window of local filesystem calls with
// nothing blocking in it — leaves an empty file no sweep collects. A stray
// byte-less file that names nobody is the cheap end of this trade; the holder
// it would otherwise cost is the expensive one.
const lockHolderMintingPrefix = "minting-"

// lockHolderMintSeq numbers this process's mints, and with the pid it makes a
// record's name unique among every record that can be live at once: two live
// processes cannot share a pid, and one process cannot draw the same number
// twice. A random name would be unique only by luck, and publishing renames
// ONTO the name it picks — so the day two draws collide, a live holder's record
// is replaced silently, which is the failure this file exists to prevent, run
// at long odds instead of avoided.
//
// [LAW:no-shared-mutable-globals] One writer, one operation, one invariant:
// every read is an Add, and the value means nothing except that it differs
// from the last.
var lockHolderMintSeq atomic.Uint64

// lockHolderRecordName is the name one acquisition publishes under.
// [LAW:one-source-of-truth] The uniqueness argument above holds only while
// every record is named here.
func lockHolderRecordName() string {
	return fmt.Sprintf("%s%d-%d", lockHolderRecordPrefix, os.Getpid(), lockHolderMintSeq.Add(1))
}

// lockHolderRecord is what one holder says about itself. It is descriptive
// only: nothing in this package branches on a record's contents, so a record
// that is somehow wrong degrades a diagnostic message and can do nothing else.
type lockHolderRecord struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	Since   time.Time `json:"since"`
}

// recordLockHolder publishes this process's claim on lockPath and returns a
// release that retires it. The returned release wraps lockRelease — the hold
// on the lock itself — so a caller has exactly one thing to defer and cannot
// retire the record while keeping the lock, or the reverse.
//
// A recording failure is loud but never fatal: refusing the lock because a
// DIAGNOSTIC could not be written would turn a working workspace into a broken
// one over a message nobody had asked for. Same demotion, and the same reason,
// as SettleCommitLockRelease's post-success release failure.
// [LAW:no-silent-failure] loud, but not a false failure.
func recordLockHolder(storageDir, lockPath string, lockRelease func() error) func() error {
	releaseRecord, err := publishLockHolder(storageDir, lockPath)
	if err != nil {
		fmt.Fprintf(lockWaitNoticeWriter, "lit: could not record this process as the holder of %s (the lock is held; only the diagnostic naming this holder is missing): %v\n", lockPath, err)
		return lockRelease
	}
	return func() error {
		// Retire the record BEFORE dropping the lock, so no instant exists in
		// which the next acquirer holds the lock while this record still
		// answers for it.
		return errors.Join(releaseRecord(), lockRelease())
	}
}

// publishLockHolder publishes this process's claim on lockPath: what this
// process would want said about it, handed to the protocol that says it.
func publishLockHolder(storageDir, lockPath string) (func() error, error) {
	dir := lockHolderDir(storageDir, lockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("ensure holder dir: %w", err)
	}
	// The binary's base name, not its path: the arguments are what tell one
	// wedged lit from another (`backlog` vs `snapshots new`), and a long
	// absolute path pushes them past the rendered length limit. Whoever needs
	// the full path has the pid, which is the actionable handle anyway.
	payload, err := json.Marshal(lockHolderRecord{
		PID:     os.Getpid(),
		Command: strings.Join(append([]string{filepath.Base(os.Args[0])}, os.Args[1:]...), " "),
		Since:   time.Now(),
	})
	if err != nil {
		return nil, fmt.Errorf("encode holder record: %w", err)
	}
	return mintHeldRecord(dir, payload)
}

// mintHeldRecord runs the four steps that make a holder record: create it,
// take the shared hold that makes it live, fill it in, and only then give it
// the name a reader sweeps. The order is the point — under that name the record
// is complete and held from the instant it exists, so no reader ever meets a
// half-made one.
func mintHeldRecord(dir string, payload []byte) (func() error, error) {
	minted, err := os.CreateTemp(dir, lockHolderMintingPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("create holder record: %w", err)
	}
	mintPath := minted.Name()
	if err := minted.Close(); err != nil {
		return nil, errors.Join(fmt.Errorf("create holder record: %w", err), retireRecord(mintPath))
	}
	// One attempt is the whole budget: this name came from CreateTemp and no
	// reader will ever open it, so the hold is uncontended by construction and
	// contention here would mean the invariant is broken, not that waiting
	// would help. [LAW:no-silent-failure]
	release, acquired, err := filelock.Acquire(context.Background(), mintPath, false, 1, 0)
	if err != nil || !acquired {
		return nil, errors.Join(fmt.Errorf("hold holder record (acquired=%v): %w", acquired, err), retireRecord(mintPath))
	}
	if err := fillHeldRecord(mintPath, payload); err != nil {
		return nil, errors.Join(err, release(), retireRecord(mintPath))
	}
	// A hold rides the inode, not the name, so the record a reader now finds
	// under recordPath is the one this call is already holding — and this is
	// the first instant any reader can see it at all.
	recordPath := filepath.Join(dir, lockHolderRecordName())
	if err := os.Rename(mintPath, recordPath); err != nil {
		return nil, errors.Join(fmt.Errorf("publish holder record: %w", err), release(), retireRecord(mintPath))
	}
	return func() error {
		return errors.Join(release(), retireRecord(recordPath))
	}, nil
}

// fillHeldRecord writes the payload into a record the caller already holds. It
// opens WITHOUT O_CREATE so that a mint something outside this package has
// removed fails here, loudly, instead of being recreated as a second file —
// one carrying this holder's identity while nothing holds it, which would go
// on to be renamed into place and read as a live holder that is not there.
// [LAW:no-silent-failure]
func fillHeldRecord(recordPath string, payload []byte) error {
	file, err := os.OpenFile(recordPath, os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open holder record to fill in: %w", err)
	}
	_, writeErr := file.Write(payload)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("write holder record: %w", err)
	}
	return nil
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
	return strings.Join(append([]string{account}, problems...), "; ")
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
// plus an account of everything that went wrong reading them. Liveness is
// decided the one way this package's discipline permits — by acquiring, which
// is right on every death mode including SIGKILL — never by a PID probe or an
// age threshold. A record whose hold this call takes had no live owner, so it
// is retired here — and that is safe to conclude only because no publisher
// ever works under a name this loop can see: a record arrives under its own
// name finished and held, by rename (lockHolderMintingPrefix).
//
// [LAW:no-silent-failure] Read failures travel back beside the holders instead
// of shrinking the account silently: a diagnostic that quietly reports fewer
// holders than exist is worse than the silence this whole file exists to end.
func readLockHolders(storageDir, lockPath string) ([]lockHolderRecord, []string) {
	dir := lockHolderDir(storageDir, lockPath)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, []string{fmt.Sprintf("holder records unreadable: %v", err)}
	}
	var holders []lockHolderRecord
	var problems []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), lockHolderRecordPrefix) {
			continue
		}
		recordPath := filepath.Join(dir, entry.Name())
		holder, problem := readLockHolder(recordPath)
		holders = append(holders, holder...)
		problems = append(problems, problem...)
	}
	return holders, problems
}

// readLockHolder resolves one record file into at most one live holder. The
// content is read before liveness is probed: reading is what a shared hold
// permits on every platform, while the exclusive probe that decides liveness
// would, on Windows, lock out the read it is deciding about.
func readLockHolder(recordPath string) ([]lockHolderRecord, []string) {
	payload, readErr := os.ReadFile(recordPath)
	// maxAttempts 1 keeps this a probe: it decides liveness and takes custody
	// of nothing, so no process ever waits on a holder record.
	release, acquired, err := filelock.Acquire(context.Background(), recordPath, true, 1, 0)
	if err != nil {
		return nil, []string{fmt.Sprintf("holder record %s unprobeable: %v", filepath.Base(recordPath), err)}
	}
	if acquired {
		// Nobody held it: the recorder died without releasing. Retire it.
		return nil, describeProblem(errors.Join(release(), retireRecord(recordPath)), "retiring dead holder record "+filepath.Base(recordPath))
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

// describeProblem renders an error as the account's problem list, and renders
// no problem when there was none — so a caller appends its result
// unconditionally instead of guarding on success.
// [LAW:dataflow-not-control-flow]
func describeProblem(err error, doing string) []string {
	if err == nil {
		return nil
	}
	return []string{fmt.Sprintf("%s: %v", doing, err)}
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

var (
	// lockWaitNoticeGrace is how long a wait stays quiet before it is worth
	// reporting: long enough that the ordinary case — two lit commands
	// overlapping for a few milliseconds — prints nothing, short enough to
	// land inside the budget of every wait an operator could mistake for a
	// hang, starting with the workspace shared lock's ~5s. The mirror
	// beacon's ~1s is deliberately below it and never prints: a wait that
	// short is over before anyone asks whether lit is wedged, and a grace
	// under a second would report the overlaps this one exists to ignore.
	// lockWaitNoticeInterval then repeats the notice, because one
	// line at the two-second mark has scrolled away long before a 15-minute
	// commit-lock budget elapses, and a wait that stops reporting itself is
	// indistinguishable from the hang this notice exists to rule out.
	//
	// Package variables, not constants, by the convention
	// engineOpenRetryMaxElapsed and transientRetryMaxAttempts already set:
	// tests whose premise is a wait must shrink the budget rather than sleep
	// through the production one.
	lockWaitNoticeGrace    = 2 * time.Second
	lockWaitNoticeInterval = 30 * time.Second

	// lockWaitNoticeWriter is where operator notices land. Stderr per the CLI
	// binding — stdout is the parseable channel, stderr the human one — which
	// is also where every other operator notice in this package goes.
	lockWaitNoticeWriter io.Writer = os.Stderr
)

// announceLockWait reports, for as long as it runs, that an acquisition is
// still waiting and who it is waiting on. The returned stop ends the reporting
// and does not return until the reporter has, so nothing prints about a wait
// that is already over.
//
// [LAW:no-ambient-temporal-coupling] The schedule is this function's own,
// owned and named; no correctness rests on it. The notice is an account of a
// wait, so its timing is the fact it reports, and a missed tick costs a line
// of output and nothing else.
func announceLockWait(ctx context.Context, storageDir, lockPath string) func() {
	stopped := make(chan struct{})
	reported := make(chan struct{})
	go func() {
		defer close(reported)
		started := time.Now()
		timer := time.NewTimer(lockWaitNoticeGrace)
		defer timer.Stop()
		for {
			select {
			case <-stopped:
				return
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			fmt.Fprintf(lockWaitNoticeWriter, "lit: still waiting for %s after %s; %s\n",
				lockPath, time.Since(started).Round(time.Second), describeLockHolders(storageDir, lockPath))
			timer.Reset(lockWaitNoticeInterval)
		}
	}()
	return func() {
		close(stopped)
		<-reported
	}
}
