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
// holds it SHARED for exactly as long as it holds the lock. Uniqueness is
// what makes the reader's cleanup safe — no future holder can ever open a name
// already minted, so unlinking a record whose hold the reader just acquired
// cannot race a live acquirer — and shared is what makes it readable: a reader
// on Windows, where LockFileEx is mandatory, can read a shared-held record but
// not an exclusively-held one.
//
// [LAW:no-ambient-temporal-coupling] A visible record is a complete record:
// content is written to a temp name and renamed into place, so "created but
// not yet filled in" is not a state a reader can observe rather than a window
// a reader is trusted to miss.
//
// NO WAIT EDGE. Every acquisition of a record's flock — the holder's own and
// every reader's probe — passes maxAttempts 1, so no process ever waits on
// one, and a lock with no inbound wait edge cannot complete a cycle. That is
// the whole of this file's standing in the package's acquisition order; it
// needs no slot, for the same reason the sync-push lock needs none.

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

// lockHolderRecordPrefix marks a file as a complete record. Content is staged
// under lockHolderTempPrefix and renamed on, so a reader that filters on this
// prefix cannot pick up a half-written stage file.
const (
	lockHolderRecordPrefix = "holder-"
	lockHolderTempPrefix   = "staging-"
)

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

// publishLockHolder writes one record and takes the shared hold that makes it
// live. Failure leaves nothing behind that a reader would report.
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
	staged, err := os.CreateTemp(dir, lockHolderTempPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("create holder record: %w", err)
	}
	recordPath := filepath.Join(dir, lockHolderRecordPrefix+strings.TrimPrefix(filepath.Base(staged.Name()), lockHolderTempPrefix))
	if err := writeAndRename(staged, payload, recordPath); err != nil {
		return nil, err
	}
	// maxAttempts 1: the name was minted by CreateTemp moments ago and no
	// other process can have opened it, so contention here is unrepresentable
	// and a non-acquisition is a real fault, not a healthy holder.
	release, acquired, err := filelock.Acquire(context.Background(), recordPath, false, 1, 0)
	if err != nil || !acquired {
		return nil, errors.Join(fmt.Errorf("hold holder record (acquired=%v): %w", acquired, err), retireRecord(recordPath))
	}
	return func() error {
		return errors.Join(release(), retireRecord(recordPath))
	}, nil
}

// retireRecord ensures a record file is gone. Already-gone is the goal
// reached, not a failure: a reader that proves a record unheld retires it, and
// it can prove exactly that about a record in the instant between its rename
// into place and its writer's hold. Two parties may therefore remove one
// record, and neither is wrong. Every other removal failure still surfaces.
// [LAW:no-silent-failure]
func retireRecord(recordPath string) error {
	if err := os.Remove(recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("retire holder record: %w", err)
	}
	return nil
}

// writeAndRename fills the staged file and moves it onto its final name, so
// the record becomes visible to readers whole or not at all. A failure clears
// the stage rather than leaving a partial file for the next crash to inherit.
func writeAndRename(staged *os.File, payload []byte, recordPath string) error {
	_, writeErr := staged.Write(payload)
	err := errors.Join(writeErr, staged.Close())
	if err == nil {
		err = os.Rename(staged.Name(), recordPath)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("write holder record: %w", err), retireRecord(staged.Name()))
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
		described = append(described, "a process that left no record (a foreign holder, or a lit older than holder records)")
	}
	account := "held by " + strings.Join(described, ", ")
	return strings.Join(append([]string{account}, problems...), "; ")
}

// readLockHolders returns the live records under lockPath's holder directory,
// plus an account of everything that went wrong reading them. Liveness is
// decided the one way this package's discipline permits — by acquiring, which
// is right on every death mode including SIGKILL — never by a PID probe or an
// age threshold. A record whose hold this call takes had no live owner, so it
// is retired here; unlinking under that hold is safe because the name is
// unique and no future acquirer will ever open it.
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
	// overlapping for a few milliseconds — prints nothing, short enough that
	// it lands inside even the briefest budget here (the workspace shared
	// lock's ~5s). lockWaitNoticeInterval then repeats the notice, because one
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
