package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dolthub/dolt/go/store/nbs"
	embedded "github.com/dolthub/driver"

	"github.com/google/uuid"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

const doltDatabaseName = "links"

// engineAccess is the engine-open contract discriminator. A write-capable open
// must end with a store that can update the journal manifest, so it fails fast
// on cross-process lock contention and retries the open (bounded) until the
// holder releases; a read open is read-only by Dolt's own contract, never
// trying the lock — reading a store someone else is writing is exactly its
// job. [LAW:types-are-the-program]
// The contract is a typed input to the one connection assembly point, not a
// per-callsite improvisation.
//
// A write open is one of two holds of Dolt's LOCK. engineWrite is the long
// hold: an open whose work can grow with its input or run past the engine
// (an import, a restore, a sync session), judged by its waiters as it always
// was, failing them after coResidentHolderWait. engineWriteRoutine is the
// routine hold: a command that makes a fixed number of issue changes, whose
// hold sends a heartbeat while its engine works (lockHold), so a hold slowed
// by a loaded machine is waited out rather than failing every writer behind
// it. There is no routine read: a read engine never holds LOCK.
type engineAccess int

const (
	engineRead engineAccess = iota
	engineWrite
	engineWriteRoutine
)

// holdsLock is whether an open of this access takes Dolt's LOCK: every write
// open, routine or long. [LAW:one-source-of-truth] the one answer every site
// that asks "does this engine hold LOCK" reads, so a write access added later
// cannot be missed by a comparison against one value.
func (a engineAccess) holdsLock() bool {
	return a != engineRead
}

// routine is whether a hold of this access sends a heartbeat (lockHold).
func (a engineAccess) routine() bool {
	return a == engineWriteRoutine
}

type Store struct {
	db *sql.DB
	// env and stderr are the environment and the operator channel of the run
	// that opened the store: the store's own git launches on its mirrors run
	// in env (mirrorGit), and every notice it prints about work it did not
	// fail lands on stderr.
	env                  environ.Env
	stderr               io.Writer
	workspaceID          string
	doltRootDir          string
	access               engineAccess
	commitLockPath       string
	commitLockStorageDir string
	telemetryDir         string
	releaseWorkspaceLock func() error
	// releaseEngineRecord retires this process's holder record for Dolt's
	// LOCK (openStoreConnection); a no-op for a read engine, which records
	// nothing. Never nil, so closeEngine runs it unconditionally.
	// [LAW:dataflow-not-control-flow]
	releaseEngineRecord func() error
	// hold is the open engine's claim on Dolt's LOCK (lockHold): what it says
	// to waiters while it lasts, and what it reports when it ends. A read
	// engine's hold never takes LOCK, so it says and reports nothing. Never
	// nil; closeEngine ends it after the engine closes.
	hold *lockHold
	// phase is the store's long phase (longPhase): every hold the store opens,
	// at open and at each rotation, is built inside it.
	phase *longPhase
	// checkedOut is the branch checkout or checkoutNewAt last put the session
	// on, "" before either ran; resumeCheckedOut is its one reader.
	checkedOut string
	// scratchDeletes is every scratch branch deleteBranch deleted or failed to
	// delete since the store opened, and scratchLooked whether anything
	// looked for one; ScratchDeletes is their one reader.
	scratchDeletes []storage.ScratchDelete
	scratchLooked  bool

	// followed is every re-root moveOntoRemoteReRoot applied since the store
	// opened; FollowedReRoots is its one reader.
	followed []storage.FollowedReRoot
	// owesCollection is whether a follow left the branch and its fork points
	// across a cut since the store last collected below one:
	// moveOntoRemoteReRoot sets it, and collectOwedCut, its one reader,
	// clears it.
	owesCollection bool
	// collections is every collection below a cut since the store opened
	// (collectOwedCut); Collections is its one reader.
	collections []storage.Collection
	// declined is every re-root cut moveOntoRemoteReRoot recognized and could
	// not follow since the store opened; DeclinedReRoots is its one reader.
	declined []storage.DeclinedReRoot
	// cutsNotFound is every follow's hold since the store opened that found
	// no cut; CutsNotFound is its one reader.
	cutsNotFound []storage.CutNotFound
	// forkPoints is what every fetch since the store opened did to each fork
	// point (recordForkPoints); ForkPointRecords is its one reader.
	forkPoints []storage.ForkPointRecord
	// pruned is every branch a fetch since the store opened found gone from
	// its remote (SyncFetch); PrunedRefs is its one reader.
	pruned []storage.PrunedRef
	// removals is every SyncRemoveRemote attempt since the store opened;
	// RemoteRemovals is its one reader.
	removals []storage.RemoteRemovalRecord
	// reconcileBases is the base every reconcile plan since the store opened
	// decided on (captureReconcilePlan); ReconcileBases is its one reader.
	reconcileBases []storage.ReconcileBase
	// strandedWritesCommits is every commit commitStrandedWrites landed since
	// the store opened, and strandedWritesLooked whether it ran;
	// StrandedWritesCommits is their one reader.
	strandedWritesCommits []storage.StrandedWritesCommit
	strandedWritesLooked  bool
	// settledLastWrites is every settle's write of settled.last since the
	// store opened (settleReceived); SettledLastWrites is its one reader.
	settledLastWrites []storage.SettledLastWrite
	// bounds is what each push since the store opened did to keep the sync
	// branch's history inside its window (boundHistory); HistoryBounds is
	// its one reader.
	bounds []storage.HistoryBound

	// sizeCeiling is the bound this store's governed parts are held under
	// (footprint.go): storeSizeCeiling, carried as a value so the retention
	// rule takes it as an input rather than reading a constant inside it.
	sizeCeiling int64

	// clock is where this store learns the instant it stamps a write with. It
	// is a construction-time dependency rather than a read from inside the
	// write path, so a caller that needs two rows whose timestamps no real
	// clock would hand out in that order can say so — which is what lets the
	// conformance suite state a time-ordering rule at all.
	// [LAW:effects-at-boundaries]
	clock storage.Clock
	// stamps turns a clock reading into the instant a write stamps, held past
	// the history the write was planned against, and counts each hold.
	stamps storage.StampTally

	// attribution stamps every event this store records with the checkout that
	// produced it. It is deliberately NOT an Open() parameter: identity is
	// resolved only after the store opens, so that a command which cannot reach
	// its store mints no identity at all — a contract app.Open owns and
	// TestFailedIdentityResolutionReleasesTheStore pins. AttributeTo is how the
	// owner completes the store once identity is known, and a store nobody
	// attributes records unattributed events, which is exactly what a store
	// opened for reading should do.
	// [LAW:no-ambient-temporal-coupling] The phase transition has one owner
	// (app.Open) rather than an ambient assumption about call order.
	attribution model.Attribution

	// observers hear every change Apply makes; see storage.ChangeObserver.
	// None is the zero value, and Apply calls each one there is.
	observers []func(storage.Change, model.Issue)

	// planReadsHookForTest, if non-nil, fires amid a write's plan reads — in
	// Apply after its GetIssue, in CreateIssue after historyFiledUnder — with
	// the commit lock held and no tx open. Production callers leave it nil; the
	// concurrency regression tests set it to attempt a foreign write there and
	// prove it cannot land until the planned write has. It sits after the
	// first read and before the rest, so a hold that leaves out the reads after
	// it — one shrunk to the write, say — leaves it outside too, and the attempt
	// lands; a hold that leaves out only the first read is beyond its reach.
	// [LAW:no-shared-mutable-globals] The seam is per-Store instance state with
	// an explicit owner, not an ambient package global — so tests holding their
	// own Store can never race on it (and t.Parallel() stays safe).
	planReadsHookForTest func()

	// commitWorkingSetHookForTest, if non-nil, fires at the top of every
	// commitWorkingSetOnce — the versioning (DOLT_COMMIT) step — and its error,
	// if any, stands in for that step failing. Production callers leave it nil;
	// the withMutation phase-resume test uses it to fail versioning transiently
	// exactly once, proving a retry resumes at versioning instead of re-running
	// the staged mutation body. Same per-Store seam rationale as above.
	commitWorkingSetHookForTest func() error

	// landHookForTest, if non-nil, fires at the top of every attempt of a
	// scratch run's landing (landScratch), and its error, if any, stands in
	// for that attempt failing. Production callers leave it nil; the batch
	// tests use it to fail a landing transiently, proving its retry rotates and
	// resumes on the branch it lands on. Same per-Store seam rationale as
	// above.
	landHookForTest func() error

	// createScratchHookForTest, if non-nil, fires at the top of every attempt
	// to create a scratch branch (runOnScratch), and its error, if any, stands
	// in for that attempt failing. Production callers leave it nil. Same
	// per-Store seam rationale as above.
	createScratchHookForTest func() error

	// moveForkPointsHookForTest, if non-nil, fires at the top of every
	// moveForkPointsAcrossTheCut, and its error, if any, stands in for that
	// move failing after a follow moved the branch. Production callers leave
	// it nil. Same per-Store seam rationale as above.
	moveForkPointsHookForTest func() error

	// migrated is what Open's migrate did; see Migrated.
	migrated storage.Migrated
}

// Open opens the store for a long write hold (engineWrite): the hold every
// write takes unless it states it is routine. Its waiters fail after
// coResidentHolderWait, naming it, as they always have.
func Open(ctx context.Context, env environ.Env, stderr io.Writer, doltRootDir string, workspaceID string) (*Store, error) {
	return openWrite(ctx, env, stderr, doltRootDir, workspaceID, engineWrite)
}

// OpenRoutine opens the store for a routine write hold (engineWriteRoutine):
// a command that makes a fixed number of issue changes. Its hold heartbeats
// while its engine works (lockHold), so a hold slowed by a loaded machine is
// waited out rather than failing the writers behind it.
func OpenRoutine(ctx context.Context, env environ.Env, stderr io.Writer, doltRootDir string, workspaceID string) (*Store, error) {
	return openWrite(ctx, env, stderr, doltRootDir, workspaceID, engineWriteRoutine)
}

// openWrite is the one write open, for either hold.
func openWrite(ctx context.Context, env environ.Env, stderr io.Writer, doltRootDir string, workspaceID string, access engineAccess) (_ *Store, err error) {
	if err := validateOpenArgs(doltRootDir, workspaceID); err != nil {
		return nil, err
	}
	// [LAW:single-enforcer] Workspace shared lock is acquired BEFORE
	// ensureDoltDatabase because its "is there a database" check and the
	// rename that installs one are decisions about the Dolt directory.
	// Acquiring after would leave a window in which `lit snapshots restore`
	// or an adopt could rotate the directory between the check and the
	// install.
	release, err := acquireWorkspaceShared(ctx, stderr, doltRootDir)
	if err != nil {
		return nil, err
	}
	// [LAW:no-silent-failure] On any failure path before the Store owns
	// the lock, release the hold AND surface a release error via the named
	// return — a leaked shared hold would block subsequent restores with
	// workspace-busy from a vanished caller. success guards the happy path
	// so the lock survives in the returned Store.
	success := false
	defer func() {
		if success {
			return
		}
		if relErr := release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	// The adopt-pending check runs HERE, after the lock is held — never
	// before. A pre-lock check is a decision made about a workspace another
	// process may still be rewriting: a caller that read "no marker", then
	// blocked on the lock while an adopt started and died mid-clone, would
	// sail into the residue on a stale answer. Under the lock the marker's
	// meaning is unambiguous — a live adopt holds the workspace lock
	// exclusively, so marker-with-acquirable-lock always means a DEAD adopt.
	// [LAW:no-ambient-temporal-coupling] the lock is the liveness owner; the
	// check's answer is only valid while it is held.
	if err = requireNoPendingAdopt(doltRootDir); err != nil {
		return nil, err
	}
	bootstrap, err := ensureDoltDatabase(ctx, stderr, doltRootDir, workspaceID)
	if err != nil {
		return nil, err
	}
	s, err := openStoreConnection(ctx, env, stderr, doltRootDir, workspaceID, access)
	if err != nil {
		return nil, err
	}
	s.releaseWorkspaceLock = release
	// [LAW:single-enforcer] Store-level commit lock is the single writer gate for all startup and runtime mutations.
	// Branch normalization runs on this open's own engine every time — the
	// bootstrap only runs it at creation — so a database that arrived
	// pre-made on another branch (an adopt's clone) is still renamed to
	// master on its first write open. [LAW:dataflow-not-control-flow] it is
	// unconditional; on an already-normalized store it reads and does nothing.
	if err = s.withCommitLock(ctx, func(ctx context.Context) error {
		if err := ensureMasterDefaultBranch(ctx, s.db); err != nil {
			return wrapEngineOpenContention(err, doltRootDir)
		}
		migrated, err := s.migrate(ctx)
		migrated.Bootstrap = bootstrap
		s.migrated = migrated
		return err
	}); err != nil {
		if closeErr := s.closeEngine(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		s.releaseWorkspaceLock = nil
		return nil, err
	}
	// The open has succeeded and nothing has been handed the Store, so this
	// engine holds LOCK and has staged no table file: the one moment the
	// staging directory is provably all dead writers' leftovers. Every app-mode
	// write command opens here, whatever its sync settings, so what any killed
	// writer left is collected by the next one. A failed collection leaves the
	// store usable and is the open's to report, not to fail on.
	collected := collectDeadTempTableFiles(doltRootDir)
	s.migrated.TempTableFiles = &collected
	success = true
	return s, nil
}

func OpenForRead(ctx context.Context, env environ.Env, stderr io.Writer, doltRootDir string, workspaceID string) (_ *Store, err error) {
	s, err := openInitialized(ctx, env, stderr, doltRootDir, workspaceID, engineRead)
	if err != nil {
		return nil, err
	}
	// The schema check is answered with reads and no commit lock: a read
	// command that queued behind that lock waited out whichever writer held
	// it — a 70ms mutation, or a receive holding the store across the
	// network — for nothing it needed, since the read engine reads beside a
	// live writer by design. [LAW:no-ambient-temporal-coupling] a reader's
	// only wait is on its own engine; it has no wait edge to any lit lock.
	assessment, fail := s.assessMigration(ctx)
	if fail != nil {
		return nil, errors.Join(fail, s.Close())
	}
	if !assessment.needsWrite() {
		s.migrated = storage.Migrated{SchemaAhead: assessment.op.schemaAhead()}
		return s, nil
	}
	// The schema trails this binary. A read never applies a migration — its
	// engine is read-only, and cannot run DDL — so the
	// workspace is brought forward by the write open, the one migration
	// boundary, and the read is served from the store that did it. The read
	// engine closes first: a process never holds two engines on the path.
	// [LAW:single-enforcer] one migrate, reached from both opens.
	// [LAW:no-silent-failure] a write open that fails here names why a read
	// command needed one, or its contention refusal reads as a read waiting
	// on a store it should never wait for.
	if err := s.Close(); err != nil {
		return nil, err
	}
	writer, err := Open(ctx, env, stderr, doltRootDir, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("this read open found %s and handed off to the write open to bring it forward: %w", assessment, err)
	}
	return writer, nil
}

// openInitialized opens an engine on an initialized workspace without
// migrating it: the shared hold every reader of the Dolt directory takes, then
// the connection. The Store owns the hold from the moment it exists, so Close
// releases both. Open and OpenSync do not use it: they create the database
// when it is missing, where this refuses.
//
// [LAW:single-enforcer] The read open and the below-the-gate entry points
// (DumpRaw, Unquarantine) reach the database through this one sequence, so the
// post-lock adopt check cannot be left out of any of them.
func openInitialized(ctx context.Context, env environ.Env, stderr io.Writer, doltRootDir string, workspaceID string, access engineAccess) (*Store, error) {
	if err := validateOpenArgs(doltRootDir, workspaceID); err != nil {
		return nil, err
	}
	release, err := acquireLiveStoreHold(ctx, stderr, doltRootDir)
	if err != nil {
		return nil, err
	}
	s, err := openStoreConnection(ctx, env, stderr, doltRootDir, workspaceID, access)
	if err != nil {
		return nil, errors.Join(err, release())
	}
	s.releaseWorkspaceLock = release
	return s, nil
}

// AttributeTo names the checkout whose work this store is about to record, so
// that every event it writes from here on carries the attribution pair. The
// caller supplies the raw stream token; pairing it with the workspace id this
// store already holds happens here, which is why no caller can pair them
// differently and no caller can supply half of one.
//
// Attribution is the store's, not each mutation's: newEvent mints every event
// the store writes, so stamping there makes "every work
// mutation carries its checkout's attribution pair" a property of the write
// path rather than an obligation on ten call sites that each have to remember.
//
// An empty token — a checkout that has never mutated, seen through a read-mode
// open — leaves the store unattributed rather than half-attributed; that is
// NewAttribution's contract, and it is why this takes no presence flag.
//
// [LAW:single-enforcer] Two callers attribute, each the moment its store is
// about to record new events: app.Open, and `lit sync reconcile resolve`, the
// one sync verb whose store mints events (the agent's merged text, landed as
// its edit). Both ensure the checkout's stream first, as any write does. The
// other openers — RebuildCandidate opens a recovery workspace, adopt probes
// one, the upgrade path reads a workspace's schema, OpenLocationForRead reaches
// foreign stores, and the rest of the sync verbs — read, or replay a dump
// through insertEventTx, which preserves the producer's attribution instead of
// stamping. A mutating operation added to any of those paths would NOT be
// attributed by anything here, and would need to answer that question for itself.
func (s *Store) AttributeTo(streamToken string) {
	s.attribution = model.NewAttribution(streamToken, s.workspaceID)
}

// ObserveChanges adds observe to the observers notify calls, once for every
// change this store makes to an issue through Apply, BulkApply's updates
// included once the batch stands.
func (s *Store) ObserveChanges(observe func(storage.Change, model.Issue)) {
	s.observers = append(s.observers, observe)
}

// EnsureDatabase bootstraps the database for standalone callers (lit init's
// non-adopt outcomes; tests). It takes the same workspace-shared lock
// Store.Open holds, for the same reason: its check-then-create must be
// serialized against a concurrent adopt's destructive window — an unlocked
// CREATE could land between an adopt's discard and clone, or be destroyed by
// it. Racing bootstraps serialize on the bootstrap lock inside
// ensureDoltDatabase, whose holder builds on a path nobody else opens.
// Open and OpenSync do NOT call this: they
// already hold the workspace lock and call ensureDoltDatabase directly, so
// the lock is never re-entered.
// [LAW:single-enforcer] one lock discipline for every write-capable
// bootstrap, owned at the entry points that hold it.
func EnsureDatabase(ctx context.Context, stderr io.Writer, doltRootDir string, workspaceID string) (_ storage.Bootstrap, err error) {
	if err := validateOpenArgs(doltRootDir, workspaceID); err != nil {
		return storage.BootstrapNone, err
	}
	release, err := acquireWorkspaceShared(ctx, stderr, doltRootDir)
	if err != nil {
		return storage.BootstrapNone, err
	}
	defer func() {
		if relErr := release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	// Post-lock, same as every open: refuse to build on an interrupted
	// adopt's residue — CREATE DATABASE IF NOT EXISTS would bless it as a
	// fresh store. [LAW:no-ambient-temporal-coupling]
	if err := requireNoPendingAdopt(doltRootDir); err != nil {
		return storage.BootstrapNone, err
	}
	return ensureDoltDatabase(ctx, stderr, doltRootDir, workspaceID)
}

// validateOpenArgs stays a pure argument check — no filesystem reads — so a
// caller reaching for it never inherits I/O failure modes. The adopt-pending
// refusal deliberately does NOT live here: it must run AFTER the caller's
// workspace lock is held (see the requireNoPendingAdopt call in each entry
// point), because a pre-lock answer can go stale while the caller waits out
// a concurrent adopt. [LAW:no-ambient-temporal-coupling]
func validateOpenArgs(doltRootDir string, workspaceID string) error {
	if _, err := validateDoltRootDir(doltRootDir); err != nil {
		return err
	}
	if strings.TrimSpace(workspaceID) == "" {
		return errors.New("workspace id is required")
	}
	return nil
}

// validateDoltRootDir validates a Dolt root path argument and returns its cleaned,
// canonical form. [LAW:single-enforcer][LAW:one-source-of-truth] One definition of
// "a usable root path", reached by every exported entry point that takes one:
// Open/OpenForRead/OpenSync/DumpRaw via validateOpenArgs, and
// Recover/PromoteCandidate/HealWorkspace directly. It rejects empty input (so a
// path never silently degrades into cwd-relative scratch, lock, and backup
// artifacts) and normalizes the rest, so equivalent paths that differ only by a
// trailing separator derive the SAME lock path, backup names, staging parent, and
// rename target — making "write the backup at one spelling, scan for it at
// another" unrepresentable.
func validateDoltRootDir(doltRootDir string) (string, error) {
	if strings.TrimSpace(doltRootDir) == "" {
		return "", errors.New("dolt root dir is required")
	}
	return filepath.Clean(doltRootDir), nil
}

// ExecRawForTest executes a raw SQL statement without acquiring the commit lock
// or calling commitWorkingSet. It exists solely for test fixtures that need to
// manipulate database state outside normal Store operations (e.g., backdating timestamps).
func (s *Store) ExecRawForTest(ctx context.Context, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

// closeEngine retires this process's holder record for LOCK and then closes
// the engine, in that order, so no instant exists in which the next opener
// holds LOCK while this record still answers for it — the order
// recordLockHolder keeps for every lit-minted lock. The driver's benign
// shutdown cancellation is normalized on the close alone, before the join,
// so it can never hide the record release's own failure.
// [LAW:single-enforcer] one close contract for every path that ends an
// engine: Close, and the open paths that fail after the engine came up.
// [LAW:no-silent-failure] The holder record is retired once: a second close
// (a deferred Close after an explicit one, a Close after a failed reopen) would
// release its file lock through the fd the first release closed.
//
// The hold ends after the close, not with the record: the close is engine
// work done under LOCK (under load, seconds of it), and the hold's heartbeat
// covers it until Dolt reports LOCK let go, which ends the hold from inside
// the close. Ending it here as well covers a close that failed before Dolt
// let go.
func (s *Store) closeEngine() error {
	release := s.releaseEngineRecord
	s.releaseEngineRecord = func() error { return nil }
	err := release()
	closeErr := s.db.Close()
	s.hold.end()
	if errors.Is(closeErr, context.Canceled) {
		closeErr = nil
	}
	return errors.Join(err, closeErr)
}

func (s *Store) Close() error {
	err := s.closeEngine()
	// db.Close is what releases Dolt's own journal lock (the engine holds it
	// for its whole lifetime), so it runs before the workspace release: the
	// next opener anywhere (this process's next command, or a waiting
	// mirror/foreground command in another process) must see this path free
	// the instant the store's lifetime ends. errors.Join keeps every failure
	// observable when db.Close and the release both fail; a leaked workspace
	// hold strands restores.
	if s.releaseWorkspaceLock != nil {
		release := s.releaseWorkspaceLock
		s.releaseWorkspaceLock = nil
		if relErr := release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}
	return err
}

// openStoreConnection builds a Store whose engine is open, not lazy: the
// ping forces the embedded engine — and with it a write engine's acquisition
// of Dolt's own journal lock — to happen here, at
// store construction, before any commit lock the store's user takes. Left
// lazy, a write store's first SQL would run inside withCommitLock and acquire
// the journal lock in the inverted commit→LOCK order the discipline in this
// package's doc (doc.go) forbids. [LAW:no-ambient-temporal-coupling]
// an engine's (and journal lock's) lifetime is the Store's lifetime by
// construction, not by whichever query happens to run first.
//
// A read engine opens Dolt read-only without trying the journal lock, so it
// contributes no wait edge to the ordering, keeps no writer out
// (TestAWriteOpensBesideAnOpenReader), and writes nothing to the chunk store
// under .dolt/noms at open or close (TestAReadOpenWritesNothing). Dolt's
// environment load around the engine still keeps .dolt/temptf: it creates the
// empty directory where one is missing and sweeps temp files older than a day.
// Being read-only for the engine's lifetime costs a reader nothing, because a
// read open never applies DDL — one that finds the schema trailing hands off
// to Open (see OpenForRead).
//
// A write engine that opened holds LOCK, so it records itself as its holder
// (recordLockHolder, retired by closeEngine before the engine lets go). A read
// engine does not: it never holds LOCK, and a record naming a reader would send
// an operator after the wrong pid. The one branch is the open contract's own
// discriminator. [LAW:dataflow-not-control-flow]
func openStoreConnection(ctx context.Context, env environ.Env, stderr io.Writer, doltRootDir string, workspaceID string, access engineAccess) (*Store, error) {
	phase := &longPhase{}
	db, hold, releaseEngineRecord, err := openEngine(ctx, stderr, doltRootDir, workspaceID, access, phase)
	if err != nil {
		return nil, err
	}
	return &Store{
		db:                  db,
		phase:               phase,
		env:                 env,
		stderr:              stderr,
		workspaceID:         workspaceID,
		doltRootDir:         doltRootDir,
		access:              access,
		releaseEngineRecord: releaseEngineRecord,
		hold:                hold,
		sizeCeiling:         storeSizeCeiling,
		clock:               storage.SystemClock,
		// Both facts about the commit lock, drawn side by side from the one
		// root, so neither is ever recovered by inverting the other.
		// [LAW:one-source-of-truth]
		commitLockPath:       commitLockPathForDolt(doltRootDir),
		commitLockStorageDir: workspaceStorageDir(doltRootDir),
		telemetryDir:         filepath.Join(filepath.Clean(doltRootDir), "telemetry"),
	}, nil
}

// openEngine opens the engine openStoreConnection describes (openHeldPool)
// and returns it with its hold of LOCK (lockHold) and the release of its LOCK
// holder record: a no-op for a read engine, which records nothing.
func openEngine(ctx context.Context, stderr io.Writer, doltRootDir string, workspaceID string, access engineAccess, phase *longPhase) (*sql.DB, *lockHold, func() error, error) {
	db, hold, err := openHeldPool(ctx, stderr, doltRootDir, workspaceID, doltDatabaseName, access, phase)
	if err != nil {
		return nil, nil, nil, err
	}
	// The engine took LOCK through Dolt's own loader, so the record is
	// published here rather than by acquireStoreLock.
	return db, hold, hold.record(ctx), nil
}

// openHeldPool opens a pool on database at doltRootDir, inside phase, and
// pings it, so the engine is open rather than lazy, its hold learns when Dolt
// takes LOCK (executingConnector.Connect) and reports the hold when Dolt lets
// it go, and the open's wait for LOCK is reported. [LAW:single-enforcer] the one engine open every pool
// lit opens on a store goes through, the store's own and the bootstrap's and
// a clone's alike. A pool opened with no database ("") takes LOCK only in the
// statement that creates one, which its caller runs under the same hold
// (lockHold.under). A contended open fails naming the holder
// (wrapEngineOpenContention).
func openHeldPool(ctx context.Context, stderr io.Writer, doltRootDir, workspaceID, database string, access engineAccess, phase *longPhase) (*sql.DB, *lockHold, error) {
	hold := newLockHold(ctx, stderr, doltRootDir, access, phase)
	db, err := openDoltPool(doltRootDir, workspaceID, database, hold)
	if err != nil {
		return nil, nil, err
	}
	pingErr := db.PingContext(ctx)
	hold.reportWait(ctx, pingErr)
	if err := wrapEngineOpenContention(pingErr, doltRootDir); err != nil {
		closeErr := db.Close()
		hold.end()
		return nil, nil, errors.Join(err, closeErr)
	}
	return db, hold, nil
}

// reconnect swaps s.db for a fresh connection with the Store's own open
// contract. Dolt's online garbage collection invalidates any SQL connection
// that was open when it ran ("this connection can no longer be used. please
// reconnect."), so callers that invoke DOLT_GC must rotate the pooled
// connection before any subsequent query. Must be called while the commit lock
// is held so no concurrent caller observes a torn s.db pointer.
//
// The new handle is opened and configured before the old one is closed, and
// its ENGINE opens only at the ping below — after prev.Close() has released
// the prior engine (and its journal lock), so the process never holds two
// engines on the path. The ping cannot ride openStoreConnection's eager-open
// path for exactly that reason: there the engine must open before the pool is
// handed out; here it must not open until the old engine is gone.
// [LAW:no-ambient-temporal-coupling]
//
// This re-open is the ONE place Dolt's journal lock is acquired while the
// commit lock is held — the inverted order this package's doc documents
// as this site's tolerated deviation. It cannot wedge, and the reason is two
// bounds and not one. Per call, the re-open fails the mutation loudly once
// the holders in front of it have stood still for coResidentHolderWait
// (holdWait) — with wrapEngineOpenContention's holder account, from the ping
// that makes the open (and its contention) surface here rather than at
// whichever query runs next. Holders cannot keep arriving while this
// mutation holds the commit lock: a write open, a snapshot copy and a
// mirror's clone each take LOCK and then wait on the commit lock, so the
// first to arrive stands still in front of this re-open until it fails; the
// only LOCK taker that skips the commit lock, RecordPushedHead, runs once
// per mirror cycle, and the next cycle begins with a clone. So the per-call
// wait is at most a few multiples of coResidentHolderWait. Across a mutation,
// this call is retryTransientGCContention's one rotation, which that function
// refuses unless the whole hold still fits commitLockWaiterBudget, pinned by
// TestRetryTransientGCContentionStopsBeforeOutlastingCommitLockWaiters. The
// per-call bound alone says nothing about the hold a commit-lock waiter
// faces, because the mutation's own work runs before it. With both in place,
// any holder this re-open waits on either releases or outlives this
// mutation's bounded failure, and the commit lock is released either way.
//
// The holder record rotates with the engine, as reopenEngine's does: the
// previous hold's record retires before its engine closes, and the next
// hold's is published once its engine holds LOCK, so the record, the arrival
// and the heartbeats a waiter reads carry one hold's name.
func (s *Store) reconnect(ctx context.Context) error {
	// [LAW:dataflow-not-control-flow] Reconnect runs unconditionally on every invocation; what varies is the Store's path/identity/access, not whether the rotation occurs.
	hold := newLockHold(ctx, s.stderr, s.doltRootDir, s.access, s.phase)
	next, err := openDoltPool(s.doltRootDir, s.workspaceID, doltDatabaseName, hold)
	if err != nil {
		return fmt.Errorf("reopen dolt: %w", err)
	}
	// The previous engine closes under the one close contract (closeEngine):
	// its record retires, then its hold ends with its close, before the next
	// engine can take LOCK, so no heartbeat stands for the gap in which LOCK
	// is free.
	closeErr := s.closeEngine()
	s.db, s.hold = next, hold
	if closeErr != nil {
		return fmt.Errorf("close prior dolt connection after reconnect: %w", closeErr)
	}
	pingErr := next.PingContext(ctx)
	hold.reportWait(ctx, pingErr)
	if err := wrapEngineOpenContention(pingErr, s.doltRootDir); err != nil {
		return fmt.Errorf("reopen dolt: %w", err)
	}
	s.releaseEngineRecord = hold.record(ctx)
	return s.resumeCheckedOut(ctx)
}

func (s *Store) CreateIssue(ctx context.Context, in storage.CreateIssueInput) (model.Issue, error) {
	if strings.TrimSpace(in.Title) == "" {
		return model.Issue{}, model.ValidationError{Message: "title is required"}
	}
	// [LAW:dataflow-not-control-flow] The zero value is data meaning
	// "unspecified"; resolving it to the task default here keeps every caller's
	// input flowing through the same construction below.
	issueType := in.IssueType
	if issueType == "" {
		issueType = model.TypeTask
	}
	parentID := strings.TrimSpace(in.ParentID)
	labels, err := canonicalizeLabels(in.Labels)
	if err != nil {
		return model.Issue{}, err
	}
	topic, err := issueid.NormalizeTopicForCreate(in.Topic)
	if err != nil {
		return model.Issue{}, err
	}
	assignee, err := model.NormalizeAssignee(in.Assignee)
	if err != nil {
		return model.Issue{}, err
	}
	// Containment, first half: a leaf is never created outside an epic. Pure,
	// so it is answered before the mutation opens. [LAW:single-enforcer]
	if err := storage.RejectLeafWithoutParent(issueType, parentID); err != nil {
		return model.Issue{}, err
	}
	createdBy := storage.Actor(in.Actor)
	var issue model.Issue
	// [LAW:no-ambient-temporal-coupling] The history the create is stamped past
	// is read under the hold its write commits in, as Apply's is, so no write
	// lands between the reading and the row stamped past it.
	if err := s.withCommitLock(ctx, func(ctx context.Context) error {
		// A new issue has no history of its own; the lane family it joins does,
		// and the create is stamped past it, as every write is. Read once the pure
		// checks pass. The parent itself is checked in the mutation, so a create
		// refused there has already taken its reading, and the tally counts it as
		// it counts any refused write.
		filed, err := s.historyFiledUnder(ctx, parentID)
		if err != nil {
			return err
		}
		if hook := s.planReadsHookForTest; hook != nil {
			hook()
		}
		now := s.stamps.Stamp(s.clock, filed)
		issue = model.Issue{
			Title:       strings.TrimSpace(in.Title),
			Description: strings.TrimSpace(in.Description),
			Prompt:      strings.TrimSpace(in.Prompt),
			Plan:        strings.TrimSpace(in.Plan),
			Priority:    in.Priority,
			IssueType:   issueType,
			Topic:       topic,
			Lane:        strings.TrimSpace(in.Lane),
			Labels:      labels,
			Assignee:    assignee,
			FollowsUp:   strings.TrimSpace(in.FollowsUp),
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		issue, err = model.HydrateRow(issue, model.StatusView{Value: model.StateOpen}, nil)
		if err != nil {
			return err
		}
		return s.withMutation(ctx, "create issue", func(ctx context.Context, tx *sql.Tx) error {
			frame, parentType, err := filingParentTx(ctx, tx, parentID)
			if err != nil {
				return err
			}
			// Containment, second half: the named parent is an epic. Read on the
			// tx with the frame, so the type and the row's presence are one
			// snapshot. [LAW:single-enforcer]
			if parentID != "" {
				if err := storage.RejectParentThatIsNotAContainer(parentID, parentType); err != nil {
					return err
				}
			}
			// The followed-up ticket must exist when the link is written, as the
			// parent must: the column has no foreign key, so this is the one place
			// a dangling follows_up is refused rather than stored and never shown.
			// Retained rows pass — a follow-up of an archived ticket is still its
			// follow-up. [LAW:single-enforcer] [LAW:no-silent-failure]
			if issue.FollowsUp != "" {
				if err := requireIssueExistsTx(ctx, tx, issue.FollowsUp); err != nil {
					return err
				}
			}
			prefix, err := issueid.NormalizeConfiguredPrefix(in.Prefix)
			if err != nil {
				return fmt.Errorf("normalize issue prefix: %w", err)
			}
			issue.ID, err = newIssueID(ctx, tx, prefix, issue.Topic, issue.Title, issue.Description, createdBy, issue.CreatedAt, parentID)
			if err != nil {
				return err
			}
			issue.Rank, err = nextRankForPlacement(ctx, tx, in.Placement, frame)
			if err != nil {
				return err
			}
			archivedCol, deletedCol := retentionColumns(issue)
			if _, err := tx.ExecContext(ctx, `INSERT INTO issues(
			id, title, description, agent_prompt, work_plan, status, priority, issue_type, topic, assignee, item_rank, lane, follows_up, created_at, updated_at, closed_at, archived_at, deleted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
				issue.ID, issue.Title, issue.Description, nullableString(issue.Prompt), issue.Plan, statusForStorage(issue), issue.Priority, issue.IssueType, issue.Topic,
				issue.AssigneeValue(), issue.Rank, issue.Lane, nullableString(issue.FollowsUp), issue.CreatedAt.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano), archivedCol, deletedCol); err != nil {
				return fmt.Errorf("insert issue: %w", err)
			}
			if parentID != "" {
				// [LAW:one-source-of-truth] Build the edge as a value and route through
				// insertRelationTx; the relations INSERT statement lives only there.
				//
				// This is the one parent edge that needs no cycle check, and the
				// reason is structural rather than a judgment call: the id was
				// minted a few lines above, so nothing in the workspace can be below
				// it yet and no parent can be its own descendant. addRelationTx
				// carries that check for the edges written against issues that
				// already have a hierarchy under them.

				parentEdge := model.Relation{
					SrcID:     issue.ID,
					DstID:     parentID,
					Type:      model.RelParentChild,
					CreatedAt: issue.CreatedAt,
					CreatedBy: createdBy,
				}
				if err := insertRelationTx(ctx, tx, parentEdge); err != nil {
					return err
				}
			}
			if err := writeLabelsTx(ctx, tx, issue.ID, nil, issue.Labels, createdBy, now); err != nil {
				return err
			}
			// CreateIssue's "created" event records the initial status as a single
			// field-change row. Containers don't carry status so no row for them.
			createChanges := []model.FieldChange{}
			if !issue.IssueType.IsContainer() {
				createChanges = append(createChanges, model.FieldChange{Field: "status", From: "", To: "open"})
			}
			// A follow-up's origin is a birth fact no later event can record, so
			// the create event carries it: `lit history` shows which ticket this
			// one was filed on. [LAW:nothing-unseen]
			if issue.FollowsUp != "" {
				createChanges = append(createChanges, model.FieldChange{Field: "follows_up", From: "", To: issue.FollowsUp})
			}
			if err := s.recordEvent(ctx, tx, issue.ID, model.ActionCreated, "issue created", createdBy, now, createChanges); err != nil {
				return err
			}
			if err := smoothRanksIfNeededTx(ctx, tx, issue.Rank); err != nil {
				return err
			}
			// A respace can replace the key that set it off.
			return tx.QueryRowContext(ctx, `SELECT item_rank FROM issues WHERE id = ?`, issue.ID).Scan(&issue.Rank)
		})
	}); err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (s *Store) ListIssues(ctx context.Context, filter storage.ListIssuesFilter) ([]model.Issue, error) {
	var where []string
	var args []any
	if !filter.IncludeArchived {
		where = append(where, "i.archived_at IS NULL")
	}
	if !filter.IncludeDeleted {
		where = append(where, "i.deleted_at IS NULL")
	}
	// [LAW:one-source-of-truth] Container DB status is dead data; the lifecycle
	// derivation in hydrateIssues is the only truth source for epic state. The
	// status filter therefore lives entirely past the hydration boundary; here we
	// only validate the requested tokens so bad input fails before the query runs.
	allowedStates, err := parseStatusFilter(filter.Statuses)
	if err != nil {
		return nil, err
	}
	// Parsed before the listing query rather than after it, so an unsupported
	// sort field costs only the ancestry read; the comparator it yields reads
	// hydrated values, so the ordering itself cannot run until the rows are back
	// and built. No ORDER BY could stand in for it: a container's state is a
	// reading of its children that no column holds — the status FILTER lives
	// past hydration for the same reason (see filterByState) — and tree order
	// reads the keys of containers the listing may have filtered out.
	// [LAW:one-source-of-truth]
	ancestry, err := loadRankAncestry(ctx, s.db)
	if err != nil {
		return nil, err
	}
	ordering, err := storage.IssueOrdering(filter.SortBy, ancestry)
	if err != nil {
		return nil, err
	}
	// [LAW:types-are-the-program] Filter types are already-parsed vocabulary,
	// so they need no defensive trim/skip.
	//
	// That parse is also what lets the next two clauses stay a single `IN` list
	// where an id list may not: model.IssueType is a closed vocabulary, so the
	// DISTINCT types a caller can name are at most len(model.IssueTypes()) —
	// five — however long the slice it passed. distinctTypes is what makes that
	// a property of this clause rather than of the callers: `--type` parses to
	// one value and internal/query dedupes, but storage.Store is an
	// engine-facing interface and a direct caller passing a repeated list would
	// otherwise get exactly the unbounded shape idSet describes.
	// [LAW:parse-dont-validate]
	if types := distinctTypes(filter.IssueTypes); len(types) > 0 {
		for _, t := range types {
			args = append(args, t)
		}
		where = append(where, "i.issue_type IN ("+placeholders(len(types))+")")
	}
	// [LAW:single-enforcer] Exclusion filter mirrors the IssueTypes positive
	// filter above, bound included; keeping both at the store boundary means
	// one definition of "which types qualify" regardless of caller.
	if types := distinctTypes(filter.ExcludeIssueTypes); len(types) > 0 {
		for _, t := range types {
			args = append(args, t)
		}
		where = append(where, "i.issue_type NOT IN ("+placeholders(len(types))+")")
	}
	// Assignee is the one filter here whose values are neither a closed
	// vocabulary nor ids — the list bounded by provenance that idSet's rule
	// names — and it stays a single clause on that narrower bound: the list is
	// assembled from `--assignee` and from `assignee:` query terms, both of
	// which a person types, and nothing derives it from the backlog. A caller
	// that did derive it from the backlog would be handing the planner the
	// quadratic shape idSet describes, so that caller joins a set of its own
	// here first, rather than being written.
	if assignees := storage.TrimmedNonEmpty(filter.Assignees); len(assignees) > 0 {
		for _, assignee := range assignees {
			args = append(args, assignee)
		}
		where = append(where, "i.assignee IN ("+placeholders(len(assignees))+")")
	}
	if filter.UpdatedAfter != nil {
		where = append(where, "i.updated_at >= ?")
		args = append(args, filter.UpdatedAfter.UTC().Format(time.RFC3339Nano))
	}
	if filter.UpdatedBefore != nil {
		where = append(where, "i.updated_at <= ?")
		args = append(args, filter.UpdatedBefore.UTC().Format(time.RFC3339Nano))
	}
	if filter.HasComments != nil {
		if *filter.HasComments {
			where = append(where, "EXISTS (SELECT 1 FROM comments c WHERE c.issue_id = i.id)")
		} else {
			where = append(where, "NOT EXISTS (SELECT 1 FROM comments c WHERE c.issue_id = i.id)")
		}
	}
	if len(filter.LabelsAll) > 0 {
		labels, err := canonicalizeLabels(filter.LabelsAll)
		if err != nil {
			return nil, err
		}
		for _, label := range labels {
			where = append(where, "EXISTS (SELECT 1 FROM labels l WHERE l.issue_id = i.id AND l.label = ?)")
			args = append(args, label)
		}
	}
	if err := s.requireIssues(ctx, filter.ParentIDs); err != nil {
		return nil, err
	}
	// The two caller-supplied id sets collapse onto ONE axis before any row is
	// read, and that is a decomposition choice rather than an optimisation.
	// "Which issues are children of these parents" is a question about the
	// relations table; answering it there instead of as an EXISTS subquery
	// inside the issue scan leaves this query with a single open-ended id set
	// to join against. [LAW:decomposition]
	selected, restricted, err := s.selectedIssueIDs(ctx, filter)
	if err != nil {
		return nil, err
	}
	for _, term := range storage.TrimmedNonEmpty(filter.SearchTerms) {
		// Five disjuncts, fixed by the five columns a search term reads, and
		// they stay five however many terms the caller supplies — an extra term
		// is an extra AND'd clause, never a longer OR. The planner's quadratic
		// range merge needs a disjunct set that GROWS with caller input, which
		// is the property this shape lacks; see idSet.
		where = append(where, "(LOWER(i.title) LIKE ? OR LOWER(i.description) LIKE ? OR LOWER(COALESCE(i.agent_prompt, '')) LIKE ? OR LOWER(i.work_plan) LIKE ? OR LOWER(i.topic) LIKE ?)")
		like := "%" + strings.ToLower(term) + "%"
		args = append(args, like, like, like, like, like)
	}
	// One query either way. An id restriction is a join against the selected
	// set rather than a clause, so the restriction to no ids at all is a join
	// against no rows, answered by the same query as any other.
	// [LAW:dataflow-not-control-flow]
	from, fromArgs := "issues i", []any{}
	if restricted {
		set, arg := newIDSet(selected).rows("sel")
		from, fromArgs = set+" JOIN issues i ON i.id = sel.id", []any{arg}
	}
	query := `SELECT ` + issueColumnsQualified + ` FROM ` + from
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	rowsOut, err := s.scanListRows(ctx, query, append(fromArgs, args...))
	if err != nil {
		return nil, err
	}
	hydrated, err := s.hydrateIssues(ctx, rowsOut)
	if err != nil {
		return nil, err
	}
	// [LAW:dataflow-not-control-flow] Sort, filter and cap always run; the helpers
	// absorb "no order", "no filter" and "no limit" as data so the body stays a
	// straight pipe.
	slices.SortStableFunc(hydrated, ordering)
	return capLimit(filterByResolution(filterByState(hydrated, allowedStates), filter.Resolutions), filter.Limit), nil
}

// distinctTypes drops repeats from an issue-type filter, which is what bounds
// the `IN` list it becomes by the size of the vocabulary rather than by the
// length of what the caller passed.
func distinctTypes(types []model.IssueType) []model.IssueType {
	out := make([]model.IssueType, 0, len(types))
	for _, t := range types {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// scanListRows runs ListIssues' query and reads its rows.
func (s *Store) scanListRows(ctx context.Context, query string, args []any) ([]issueRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w (query=%s)", err, query)
	}
	defer rows.Close()
	out := []issueRow{}
	for rows.Next() {
		issue, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// selectedIssueIDs folds ListIssues' two id-shaped filters into the single set
// of ids the scan may return, and says whether they narrow anything at all.
//
// restricted is not len(ids) > 0: a caller that asked for children of a
// childless parent, or for the intersection of two disjoint id sets, narrowed
// the result to nothing, and an empty set must mean "no rows" rather than "no
// filter". A filter holding only blank ids is the opposite case — it narrows
// nothing, because every entry was dropped before the count was taken.
// [LAW:parse-dont-validate]
//
// The ids are sorted so that the row order is a function of the id set
// alone, not of the order a caller happened to type or the order the
// relations table happened to store. ListIssues' comparator runs over the rows
// afterwards and is stable, so this is what keeps the order of equal-sorting
// rows the same from call to call.
func (s *Store) selectedIssueIDs(ctx context.Context, filter storage.ListIssuesFilter) ([]string, bool, error) {
	selected := storage.TrimmedNonEmpty(filter.IDs)
	restricted := len(selected) > 0
	if len(filter.ParentIDs) > 0 {
		children, err := s.childIDsOfParents(ctx, filter.ParentIDs)
		if err != nil {
			return nil, false, err
		}
		if restricted {
			// Intersected through a set rather than slices.Contains, which
			// would be O(len(children) x len(selected)) — the same quadratic
			// over two caller-supplied id lists that idSet keeps out of the
			// planner, in Go where no index hides it.
			// The rule beside idSet governs an id set wherever it is tested,
			// not only where it is rendered into SQL.
			// [LAW:one-type-per-behavior]
			keep := make(map[string]bool, len(selected))
			for _, id := range selected {
				keep[id] = true
			}
			children = slices.DeleteFunc(children, func(id string) bool { return !keep[id] })
		}
		selected, restricted = children, true
	}
	slices.Sort(selected)
	return slices.Compact(selected), restricted, nil
}

// childIDsOfParents returns the ids of the direct children of the named
// parents.
//
// This is the parent filter's whole implementation. The membership test it
// carries is over dst_id, which is the leading column of
// idx_relations_dst_type, so the join is a run of point lookups rather
// than the correlated subquery an EXISTS form would plan once per outer row.
func (s *Store) childIDsOfParents(ctx context.Context, parentIDs []string) ([]string, error) {
	parents, arg := newIDSet(parentIDs).rows("parents")
	rows, err := s.db.QueryContext(ctx, `SELECT r.src_id FROM `+parents+` JOIN relations r ON r.dst_id = parents.id WHERE r.type = ?`, arg, string(model.RelParentChild))
	if err != nil {
		return nil, fmt.Errorf("list children of parents: %w", err)
	}
	return scanIDColumn(rows)
}

// scanIDColumn reads a single-column id result and closes it.
func scanIDColumn(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// requireIssues answers NotFoundError for the first id naming no issue row, in
// the order given. It reads existence only — a soft-deleted issue still exists —
// so it agrees with GetIssue about what an id names.
func (s *Store) requireIssues(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	named, arg := newIDSet(ids).rows("named")
	rows, err := s.db.QueryContext(ctx, `SELECT i.id FROM `+named+` JOIN issues i ON i.id = named.id`, arg)
	if err != nil {
		return fmt.Errorf("check issues exist: %w", err)
	}
	present, err := scanIDColumn(rows)
	if err != nil {
		return err
	}
	found := make(map[string]bool, len(present))
	for _, id := range present {
		found[id] = true
	}
	// Reported in the caller's order, over the caller's list, so a repeated id
	// is still named once.
	for _, id := range ids {
		if !found[id] {
			return storage.NotFoundError{Entity: "issue", ID: id}
		}
	}
	return nil
}

func parseStatusFilter(input []model.State) ([]model.State, error) {
	out := make([]model.State, 0, len(input))
	for _, raw := range input {
		state := model.DefaultOpen(string(raw))
		out = append(out, state)
	}
	return out, nil
}

// filterByState keeps only issues whose lifecycle State() is in allowed; an
// empty allow-list passes everything through so callers can route every list
// through one boundary that always reads State(), never the DB column.
// [LAW:single-enforcer] Status filtering happens against derived lifecycle
// state because container DB status is dead data; hydration is the truth source.
func filterByState(issues []model.Issue, allowed []model.State) []model.Issue {
	if len(allowed) == 0 {
		return issues
	}
	allow := make(map[model.State]struct{}, len(allowed))
	for _, state := range allowed {
		allow[state] = struct{}{}
	}
	out := make([]model.Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := allow[issue.State()]; ok {
			out = append(out, issue)
		}
	}
	return out
}

// filterByResolution keeps only issues whose recorded close resolution is in
// allowed; an empty allow-list passes everything through. Resolution lives in
// the lifecycle, so the filter reads the derived ResolutionValue() rather than
// the DB column — the same post-hydration discipline as filterByState. An issue
// with no resolution (open, in_progress, or a `done`/legacy close) matches no
// non-empty allow-list. [LAW:single-enforcer]
func filterByResolution(issues []model.Issue, allowed []model.Resolution) []model.Issue {
	if len(allowed) == 0 {
		return issues
	}
	allow := make(map[model.Resolution]struct{}, len(allowed))
	for _, r := range allowed {
		allow[r] = struct{}{}
	}
	out := make([]model.Issue, 0, len(issues))
	for _, issue := range issues {
		resolution := issue.ResolutionValue()
		if resolution == nil {
			continue
		}
		if _, ok := allow[*resolution]; ok {
			out = append(out, issue)
		}
	}
	return out
}

// capLimit truncates issues to the first n entries. limit <= 0 means uncapped,
// matching the existing ListIssuesFilter.Limit convention; the helper exists so
// the LIMIT semantic is one expression at the boundary rather than a branch in
// the body.
func capLimit(issues []model.Issue, limit int) []model.Issue {
	if limit <= 0 || len(issues) <= limit {
		return issues
	}
	return issues[:limit]
}

func (s *Store) GetIssueDetail(ctx context.Context, id string) (model.IssueDetail, error) {
	issue, err := s.GetIssue(ctx, id)
	if err != nil {
		return model.IssueDetail{}, err
	}
	relations, err := s.listRelations(ctx, id)
	if err != nil {
		return model.IssueDetail{}, err
	}
	comments, err := s.listComments(ctx, id)
	if err != nil {
		return model.IssueDetail{}, err
	}
	events, err := s.ListEvents(ctx, id)
	if err != nil {
		return model.IssueDetail{}, err
	}
	planReviews, err := s.ListPlanReviews(ctx, id)
	if err != nil {
		return model.IssueDetail{}, err
	}

	// [LAW:single-enforcer] Hydrate every related issue in one query
	// rather than running N+1 GetIssue calls. The map lets the relation
	// loop below stay a pure data-flow over already-hydrated rows. The
	// redirect target joins the same batch — hydrating it costs no extra
	// query.
	relatedIDs := collectRelatedIssueIDs(id, relations)
	if target := issue.RedirectTargetValue(); target != nil && !slices.Contains(relatedIDs, *target) {
		relatedIDs = append(relatedIDs, *target)
	}
	// The follow-up link in both directions joins the same batch: the ticket
	// this one follows up, and every ticket filed as a follow-up of this one.
	followUpIDs, err := s.followUpIDsOf(ctx, id)
	if err != nil {
		return model.IssueDetail{}, err
	}
	for _, other := range append(followUpIDs, issue.FollowsUp) {
		if other != "" && !slices.Contains(relatedIDs, other) {
			relatedIDs = append(relatedIDs, other)
		}
	}
	relatedByID, err := s.getIssuesByIDs(ctx, relatedIDs)
	if err != nil {
		return model.IssueDetail{}, err
	}
	ancestry, err := loadRankAncestry(ctx, s.db)
	if err != nil {
		return model.IssueDetail{}, err
	}

	// [LAW:one-source-of-truth] Structural edges (parent/child/blocks) are
	// bucketed by the same helper the batch accessor uses, so the blocks
	// convention has one definition. Related is GetIssueDetail's own concern.
	structural := bucketRelations(id, relations, relatedByID, ancestry)
	// Siblings are the parent's other children. The set exists only when the
	// issue has a parent; an only child yields the empty slice and the renderer
	// omits the group. [LAW:one-source-of-truth] derived from the same
	// rank-ordered children query other consumers read, minus self.
	siblings := []model.Issue{}
	if structural.Parent != nil {
		// Every retention state, as the children edge records it: a sibling
		// archived out of the listing is still the parent's child.
		parentChildren, err := s.ListIssues(ctx, storage.ListIssuesFilter{
			ParentIDs:       []string{structural.Parent.ID},
			IncludeArchived: true,
			IncludeDeleted:  true,
		})
		if err != nil {
			return model.IssueDetail{}, err
		}
		siblings = siblingsOf(id, parentChildren)
	}
	// The redirect target hydrates from the issue's own column — related-to
	// edges mean exactly one thing (manual peer links), so there is nothing to
	// re-derive. A target whose row has vanished hydrates as absent, matching
	// getIssuesByIDs' hole-skipping contract.
	var redirectTarget *model.Issue
	if target := issue.RedirectTargetValue(); target != nil {
		if hydrated, ok := relatedByID[*target]; ok {
			redirectTarget = &hydrated
		}
	}
	related := relatedFrom(id, relations, relatedByID, ancestry)
	// Both readings of follows_up hydrate from the row, never from the graph;
	// a followed-up ticket whose row has vanished hydrates as absent, matching
	// getIssuesByIDs' hole-skipping contract. [LAW:one-source-of-truth]
	var followsUp *model.Issue
	if hydrated, ok := relatedByID[issue.FollowsUp]; ok {
		followsUp = &hydrated
	}
	followUps := []model.Issue{}
	for _, other := range followUpIDs {
		if hydrated, ok := relatedByID[other]; ok {
			followUps = append(followUps, hydrated)
		}
	}
	ancestry.Sort(followUps)
	detail := model.IssueDetail{
		Issue:          issue,
		Relations:      relations,
		Comments:       comments,
		Events:         events,
		PlanReviews:    planReviews,
		Children:       structural.Children,
		Siblings:       siblings,
		BlockedBy:      structural.BlockedBy,
		Blocks:         structural.Blocks,
		Parent:         structural.Parent,
		Related:        related,
		RedirectTarget: redirectTarget,
		FollowsUp:      followsUp,
		FollowUps:      followUps,
	}
	return detail, nil
}

// followUpIDsOf returns the ids of every ticket filed as a follow-up of
// focalID — the reverse reading of issues.follows_up. Every retention state,
// as the column records it: an archived follow-up is still a follow-up.
func (s *Store) followUpIDsOf(ctx context.Context, focalID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM issues WHERE follows_up = ?`, focalID)
	if err != nil {
		return nil, fmt.Errorf("list follow-ups of %s: %w", focalID, err)
	}
	return scanIDColumn(rows)
}

// collectRelatedIssueIDs returns every distinct counterparty id referenced
// by relations, excluding the focal id itself.
func collectRelatedIssueIDs(focalID string, relations []model.Relation) []string {
	seen := make(map[string]struct{}, len(relations)*2)
	ids := make([]string, 0, len(relations))
	add := func(candidate string) {
		if candidate == "" || candidate == focalID {
			return
		}
		if _, exists := seen[candidate]; exists {
			return
		}
		seen[candidate] = struct{}{}
		ids = append(ids, candidate)
	}
	for _, rel := range relations {
		add(rel.SrcID)
		add(rel.DstID)
	}
	return ids
}

// getIssuesByIDs batch-loads issues by id and returns a map keyed by id.
// Missing ids (deleted/archived/never-existed) are simply absent from the
// returned map; callers decide whether absence is an error or merely a hole
// to skip. Empty input returns an empty map without querying.
func (s *Store) getIssuesByIDs(ctx context.Context, ids []string) (map[string]model.Issue, error) {
	if len(ids) == 0 {
		return map[string]model.Issue{}, nil
	}
	named, arg := newIDSet(ids).rows("named")
	scanned, err := s.scanIssueRows(ctx, `SELECT `+issueColumnsQualified+` FROM `+named+` JOIN issues i ON i.id = named.id`, []any{arg})
	if err != nil {
		return nil, err
	}
	hydrated, err := s.hydrateIssues(ctx, scanned)
	if err != nil {
		return nil, err
	}
	out := make(map[string]model.Issue, len(hydrated))
	for _, issue := range hydrated {
		out[issue.ID] = issue
	}
	return out, nil
}

// issueTypesByIDs reads each id's issue_type as a column, keyed by id, with
// getIssuesByIDs' absence contract. It hydrates nothing, so it answers on a
// hierarchy holding a parent loop, which hydration refuses
// (storage.ParentCycleError) — the state a caller clearing a parent is there
// to repair, as ClearParent explains.
func (s *Store) issueTypesByIDs(ctx context.Context, ids []string) (map[string]model.IssueType, error) {
	named, arg := newIDSet(ids).rows("named")
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, i.issue_type FROM `+named+` JOIN issues i ON i.id = named.id`, arg)
	if err != nil {
		return nil, fmt.Errorf("load issue types: %w", err)
	}
	defer rows.Close()
	types := make(map[string]model.IssueType, len(ids))
	for rows.Next() {
		var id string
		var issueType model.IssueType
		if err := rows.Scan(&id, &issueType); err != nil {
			return nil, fmt.Errorf("scan loaded issue type: %w", err)
		}
		types[id] = issueType
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate loaded issue types: %w", err)
	}
	return types, nil
}

// scanIssueRows runs an issue-row query and reads its rows.
func (s *Store) scanIssueRows(ctx context.Context, query string, args []any) ([]issueRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load issues by id: %w", err)
	}
	defer rows.Close()
	scanned := []issueRow{}
	for rows.Next() {
		row, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("scan loaded issue: %w", err)
		}
		scanned = append(scanned, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate loaded issues: %w", err)
	}
	return scanned, nil
}

func (s *Store) GetIssue(ctx context.Context, id string) (model.Issue, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+issueColumnsBare+` FROM issues WHERE id = ?`, id)
	scanned, err := scanIssue(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Issue{}, storage.NotFoundError{Entity: "issue", ID: id}
		}
		return model.Issue{}, err
	}
	hydrated, err := s.hydrateIssues(ctx, []issueRow{scanned})
	if err != nil {
		return model.Issue{}, err
	}
	return hydrated[0], nil
}

// fieldWrite is a fully-planned field mutation, ready to execute against a tx.
// planFieldUpdate computes it (pure: validation + in-memory mutation + the
// change-row diff); applyFieldsTx performs the writes. The plan/apply split is
// what lets Apply run a field write inside the same transaction as a
// status transition, so a combined update lands as one Dolt commit instead of
// two. [LAW:decomposition] [LAW:effects-at-boundaries]
type fieldWrite struct {
	issue model.Issue
	// priorLabels is the set the write moves the issue's label rows from.
	priorLabels []string
	actor       string
	reason      string
	changes     []model.FieldChange
}

// planFieldUpdate dresses the contract's field decision as this engine's write
// plan. What a patch means — which fields it may touch, what they validate to,
// which of them history reports — is [storage.ApplyIssueFields], because every
// engine owes lit the same answer there; what is left here is the Dolt-side
// packaging. baseline is Apply's read of the row, or the post-action issue when
// the change also carries a lifecycle action — so a start's new assignee is the
// diff's prior, not a second assignee change row. [LAW:effects-at-boundaries] A
// pure function of (baseline, in, actor): no clock, no IO. The UpdatedAt stamp
// and every write are deferred to applyFieldsTx.
func planFieldUpdate(baseline model.Issue, in storage.UpdateIssueInput, actor string) (fieldWrite, error) {
	issue, changes, err := storage.ApplyIssueFields(baseline, in)
	if err != nil {
		return fieldWrite{}, err
	}
	return fieldWrite{issue: issue, priorLabels: baseline.Labels, actor: actor, reason: in.Reason, changes: changes}, nil
}

// applyFieldsTx writes a planned field mutation that moved at least one field
// against tx: the issue UPDATE, the label rows the set's move adds and drops, and
// the change event. It owns only writes — every decision was made in planFieldUpdate — so
// it composes into any transaction a caller already holds. [LAW:single-enforcer]
// The UPDATE sets only the columns the field axis owns: the lifecycle columns
// (status, closed_at, archived_at, deleted_at) belong to the transition and
// retention writes, so each column has one writer and a field write restates
// no lifecycle column. [LAW:one-source-of-truth]
func (s *Store) applyFieldsTx(ctx context.Context, tx *sql.Tx, w fieldWrite, at time.Time) error {
	// [LAW:effects-at-boundaries] The instant arrives with the write, not in
	// planFieldUpdate — so the plan stays a pure function of its inputs.
	issue, event := s.stampEdit(w.issue, at, w.reason, w.actor, w.changes)
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET
		title = ?, description = ?, agent_prompt = ?, work_plan = ?, priority = ?, issue_type = ?, assignee = ?, lane = ?, updated_at = ?
		WHERE id = ?`, issue.Title, issue.Description, nullableString(issue.Prompt), issue.Plan, issue.Priority, issue.IssueType, issue.AssigneeValue(), issue.Lane, issue.UpdatedAt.Format(time.RFC3339Nano), issue.ID); err != nil {
		return fmt.Errorf("update issue: %w", err)
	}
	// [LAW:dataflow-not-control-flow] The label rows are always moved; a set
	// the patch left as it was is a delta with nothing in it.
	if err := writeLabelsTx(ctx, tx, issue.ID, w.priorLabels, issue.Labels, w.actor, at); err != nil {
		return err
	}
	return insertEventTx(ctx, tx, event)
}

// [LAW:dataflow-not-control-flow] Apply is the single execution path for issue-record changes.
// Variability lives in the Change value: nil Action = no transition; empty Fields = no field write.
// [LAW:types-are-the-program] Every target state is reachable by exactly one action variant;
// no compound chains, no from-state preconditions. The 3x3 minus diagonal collapses to one call per change.
// Same-state transitions flow through planStatusTransition unconditionally; the leaf decides what they
// mean. A pure no-op (same state, same resulting claimant) records nothing — history reflects actual
// mutations. A same-state start that moves the claimant is the canonical reclaim path and records the
// move with the calling Actor, which is the audit substrate for "who interacted with this ticket"
// history queries. The claimant is the (assignee, checkout) pair, never the assignee alone: two
// checkouts of one identity taking a lane from each other move nothing on the row and everything on
// the record.
func (s *Store) Apply(ctx context.Context, id string, plan storage.ChangePlan) (storage.Applied, error) {
	c, applied, err := s.apply(ctx, id, plan)
	if err != nil {
		return storage.Applied{}, err
	}
	s.notify(ctx, c, applied.Issue)
	return applied, nil
}

// owed is something an observer is owed once the write behind it stands.
type owed func()

// hearingKey marks a context whose writes may yet not stand; its value is the
// queue of what their observers are owed.
type hearingKey struct{}

// notify is the one place a change reaches the observers.
func (s *Store) notify(ctx context.Context, c storage.Change, issue model.Issue) {
	hear(ctx, func() {
		for _, observe := range s.observers {
			observe(c, issue)
		}
	})
}

// hear owes the observers what happened. Inside a mutation's attempt or a batch
// it is queued, and each hands its queue on only for writes that stand: an
// attempt once its tx commits, a batch once its lock is released, so a write
// that did not land is heard by nobody. [LAW:single-enforcer]
func hear(ctx context.Context, heard ...owed) {
	if queue, ok := ctx.Value(hearingKey{}).(*[]owed); ok {
		*queue = append(*queue, heard...)
		return
	}
	for _, h := range heard {
		h()
	}
}

// apply writes the change plan decides and tells no observer; Apply notifies
// with the change it returns.
func (s *Store) apply(ctx context.Context, id string, plan storage.ChangePlan) (storage.Change, storage.Applied, error) {
	// [LAW:no-ambient-temporal-coupling] The read, the stamp, the plan and the
	// write share one commit-lock hold, so no write lands between what the plan
	// read and what it writes: the holder a start takes the ticket from, the
	// instant it is stamped at, and whether any write is owed at all are all
	// answers about the state the write lands on. Every writer of the store
	// takes this lock, so under the hold the reads see exactly what the tx
	// will. They run on s.db before the tx opens because the pool has one
	// connection, which an open tx would hold.
	var c storage.Change
	var applied storage.Applied
	if err := s.withCommitLock(ctx, func(ctx context.Context) error {
		current, err := s.GetIssue(ctx, id)
		if err != nil {
			return err
		}
		if c, err = plan.PlanChange(current); err != nil {
			return err
		}
		if hook := s.planReadsHookForTest; hook != nil {
			hook()
		}
		// [LAW:one-source-of-truth] One read of history plans the change and
		// stamps it: the issue's own events decide its holder and its phase, and
		// the lane family's decide the instant, so the write is stamped past
		// exactly the history it was planned against.
		family, err := s.laneFamilyHistory(ctx, current.ID)
		if err != nil {
			return err
		}
		at := s.stamps.Stamp(s.clock, family)
		history := eventsOf(current.ID, family)
		// [LAW:no-ambient-temporal-coupling] The combined transition+field update
		// has exactly one atomicity owner: this method plans both writes (pure:
		// validation, in-memory mutation) and then performs them inside a SINGLE
		// withMutation, so they share one SQL transaction and one Dolt commit. A
		// validation error or crash between the two writes cannot leave a
		// status-moved-but-fields-unwritten row — the torn state is
		// unrepresentable.
		// [LAW:one-source-of-truth] One change has one author: the actor is
		// normalized once here and recorded on every event the change produces.
		actor := storage.Actor(c.Actor)
		baseline := current
		var lw lifecycleWrite
		var handover *claims.Handover
		if c.Action != nil {
			lw, err = s.planLifecycleAction(current, history, at, actor, strings.TrimSpace(c.Reason), c.Action)
			if err != nil {
				return err
			}
			// The field write starts from the post-action issue so a start's new
			// assignee is restated, not overwritten, and the diff sees it as prior.
			baseline = lw.postIssue()
			handover = lw.handover()
		}
		// Every patch is planned, an empty one included, and the field axis is
		// written when the plan moved a field: a patch restating what the issue
		// holds writes no row, stamps no updated_at, and makes no commit, which is
		// what lets a bulk file read out of `lit ls --format yaml` go back in
		// unedited as a no-op. [LAW:dataflow-not-control-flow]
		fw, err := planFieldUpdate(baseline, c.Fields, actor)
		if err != nil {
			return err
		}
		hasFields := len(fw.changes) > 0
		needsActionWrite := lw != nil && !lw.isNoop()
		if needsActionWrite || hasFields {
			if err := s.withMutation(ctx, "apply update", func(ctx context.Context, tx *sql.Tx) error {
				if needsActionWrite {
					if err := lw.applyTx(ctx, s, tx); err != nil {
						return err
					}
				}
				if hasFields {
					if err := s.applyFieldsTx(ctx, tx, fw, at); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
		issue, err := s.GetIssue(ctx, id)
		if err != nil {
			return err
		}
		applied = storage.Applied{Issue: issue, Handover: handover, Fields: fw.changes}
		return nil
	}); err != nil {
		return storage.Change{}, storage.Applied{}, err
	}
	return c, applied, nil
}

// AddComment returns the commented-on issue alongside the comment: it already
// reads the issue to validate it exists, and a comment never changes the
// issue row, so that read is the caller's answer too — never a second
// round-trip to re-fetch what this call already has in hand.
// [LAW:one-source-of-truth]
func (s *Store) AddComment(ctx context.Context, in storage.AddCommentInput) (model.Comment, model.Issue, error) {
	body, err := in.Body.Text()
	if err != nil {
		return model.Comment{}, model.Issue{}, err
	}
	issue, err := s.GetIssue(ctx, in.IssueID)
	if err != nil {
		return model.Comment{}, model.Issue{}, err
	}
	now := s.clock.Now()
	comment := model.Comment{ID: "cmt-" + uuid.NewString(), IssueID: in.IssueID, Body: body, CreatedAt: now, CreatedBy: storage.Actor(in.CreatedBy)}
	if err := s.withMutation(ctx, "add comment", func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO comments(id, issue_id, body, created_at, created_by) VALUES (?, ?, ?, ?, ?)`, comment.ID, comment.IssueID, comment.Body, comment.CreatedAt.Format(time.RFC3339Nano), comment.CreatedBy); err != nil {
			return fmt.Errorf("insert comment: %w", err)
		}
		return nil
	}); err != nil {
		return model.Comment{}, model.Issue{}, err
	}
	return comment, issue, nil
}

func (s *Store) DeleteComment(ctx context.Context, commentID string) (model.Comment, error) {
	id := strings.TrimSpace(commentID)
	if id == "" {
		return model.Comment{}, model.ValidationError{Message: "comment id is required"}
	}
	var deleted model.Comment
	// [LAW:single-enforcer] Existence is verified by the SELECT inside the same mutation that deletes,
	// so the row proven present is the row removed — no TOCTOU gap, no separate guard.
	if err := s.withMutation(ctx, "delete comment", func(ctx context.Context, tx *sql.Tx) error {
		found, err := queryComments(ctx, tx, `comments WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("read comment: %w", err)
		}
		if len(found) == 0 {
			// [LAW:one-type-per-behavior] Typed not-found error matches GetIssue / relation removals,
			// so callers detect not-found via errors.As instead of string-matching the message.
			return storage.NotFoundError{Entity: "comment", ID: id}
		}
		deleted = found[0]
		// [LAW:single-enforcer] one comments DELETE, shared with the replay's delta.
		if _, err := deleteCommentTx(ctx, tx, id); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return model.Comment{}, err
	}
	return deleted, nil
}

// lifecycleWrite is the planned lifecycle mutation Apply executes — a sealed
// two-variant sum mirroring the action sum's axis split: transitionWrite for
// StatusActions, retentionWrite for the retention actions. Apply holds exactly
// one plan whichever axis the action drives, so the axis is a value flowing
// through one seam, not a second code path. [LAW:dataflow-not-control-flow]
type lifecycleWrite interface {
	// applyTx performs the planned writes against tx; it owns only writes, so
	// it composes into the transaction Apply already holds.
	applyTx(ctx context.Context, s *Store, tx *sql.Tx) error
	// postIssue is the post-action issue a following field write baselines on.
	postIssue() model.Issue
	// isNoop reports that the plan owes no write.
	isNoop() bool
	// handover is the claim the planned write hands from one holder to
	// another, nil for every plan that hands none.
	handover() *claims.Handover
}

// planLifecycleAction plans any lifecycle action by its axis: StatusActions
// travel the status state machine, RetentionActions travel the Retain
// transition table, and SetPhase moves an in-progress ticket's phase. The
// three partition the sealed sum, so the dispatch is the sum's own structure —
// a retention action reaching the status machine, or vice versa, is
// unrepresentable. [LAW:types-are-the-program]
func (s *Store) planLifecycleAction(issue model.Issue, history []model.IssueEvent, at time.Time, actor string, reason string, action model.Action) (lifecycleWrite, error) {
	switch a := action.(type) {
	case model.StatusAction:
		w, err := s.planStatusTransition(issue, history, at, actor, reason, a)
		if err != nil {
			return nil, err
		}
		return w, nil
	case model.RetentionAction:
		w, err := planRetentionTransition(issue, actor, reason, a, at)
		if err != nil {
			return nil, err
		}
		return w, nil
	case model.SetPhase:
		w, err := s.planPhaseMove(issue, history, at, actor, reason, a)
		if err != nil {
			return nil, err
		}
		return w, nil
	default:
		// [LAW:no-silent-failure] The sealed sum has no fourth axis; only an
		// impostor Action reaches here.
		panic(fmt.Sprintf("illegal Action value %T", action))
	}
}

// phaseWrite is a planned phase move. The phase is derived from the issue's
// history (model.PhaseOf), so the move's whole record is its event; the row
// moves only updated_at, at the event's instant. Issue.PlanPhase refuses a
// ticket that is not in progress, and the plan reads the ticket under the hold
// its write lands in, so the status it planned against is the status written on.
type phaseWrite struct {
	move  model.PhaseChange
	event model.IssueEvent
	post  model.Issue
}

func (w phaseWrite) postIssue() model.Issue     { return w.post }
func (w phaseWrite) isNoop() bool               { return !w.move.Changed() }
func (w phaseWrite) handover() *claims.Handover { return nil }

func (w phaseWrite) applyTx(ctx context.Context, s *Store, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET updated_at = ? WHERE id = ?`,
		w.event.CreatedAt.Format(time.RFC3339Nano), w.event.IssueID); err != nil {
		return fmt.Errorf("update issue for phase move: %w", err)
	}
	return insertEventTx(ctx, tx, w.event)
}

// planPhaseMove plans the move against the issue's history through
// Issue.PlanPhase, the rule both engines share. [LAW:single-enforcer] A move
// mints its event here, so the post-move issue a following field write
// baselines on carries the event's instant, as the memory engine's does.
func (s *Store) planPhaseMove(issue model.Issue, history []model.IssueEvent, at time.Time, actor, reason string, action model.SetPhase) (phaseWrite, error) {
	move, err := issue.PlanPhase(history, action.Phase)
	if err != nil {
		return phaseWrite{}, err
	}
	if !move.Changed() {
		return phaseWrite{move: move, post: issue}, nil
	}
	event := s.newEvent(issue.ID, string(model.ActionPhase), reason, actor, at, move.FieldChanges())
	post := issue
	post.UpdatedAt = event.CreatedAt
	return phaseWrite{move: move, event: event, post: post}, nil
}

// transitionWrite is a fully-planned status transition, ready to execute
// against a tx. planStatusTransition is the validate+compute half — it runs
// Issue.Apply's guards, the assignee rule, and the change-row diff over the
// issue, the history and the instant Apply read for it, and reads and writes
// nothing itself. applyTransitionTx is the write half: the redirect-target
// validation, which reads a foreign row on the write's tx, then the single
// UPDATE plus the event. The plan also carries `post` — the
// post-transition issue — which Apply uses as the baseline for a following
// field write. A noop plan records that the target state and assignee already
// hold, so no write is owed. [LAW:decomposition]
type transitionWrite struct {
	issueID           string
	toStatus          string
	postAssignee      string
	now               time.Time
	closedAtArg       any
	resolutionArg     any
	redirectTargetArg any
	action            model.ActionName
	reason            string
	actor             string
	changes           []model.FieldChange
	post              model.Issue
	noop              bool
	claimHandover     *claims.Handover
}

func (w transitionWrite) applyTx(ctx context.Context, s *Store, tx *sql.Tx) error {
	return s.applyTransitionTx(ctx, tx, w)
}
func (w transitionWrite) postIssue() model.Issue     { return w.post }
func (w transitionWrite) isNoop() bool               { return w.noop }
func (w transitionWrite) handover() *claims.Handover { return w.claimHandover }

func (s *Store) planStatusTransition(issue model.Issue, history []model.IssueEvent, at time.Time, actor string, reason string, action model.StatusAction) (transitionWrite, error) {
	updated, err := issue.Apply(action)
	if err != nil {
		return transitionWrite{}, err
	}
	// Who holds the ticket is the (assignee, checkout) pair claims owns, read
	// from this issue's own history — never the assignee alone. The history read
	// is what makes the checkout half available: ownership is recorded on events,
	// not on the row, so there is no column to compare. [LAW:one-source-of-truth]
	// s.attribution is the taker for the same reason it is the right one to
	// compare against: it is exactly what recordEvent will stamp on the event
	// this plan is deciding whether to write.
	prior := claims.ClaimantOf(issue, history)
	post := prior.After(action, s.attribution)
	updated.Assignee = post.Assignee
	fromStatus := issue.StatusValue()
	toStatus := updated.StatusValue()
	// [LAW:one-source-of-truth] History records actual mutations only. A call
	// whose target state and resulting claimant both match the record is the
	// documented leaf-state Apply no-op: no write, no event. The claim audit
	// substrate survives because a reclaim — same state, new assignee OR a new
	// checkout taking the lane — moves the claimant and falls through, which is
	// what keeps "who took this over" answerable. Comparing only the assignee
	// would drop every takeover between two checkouts of one identity on the
	// floor, silently and with exit 0. [LAW:no-silent-failure]
	if toStatus == fromStatus && post == prior {
		return transitionWrite{noop: true, post: issue}, nil
	}
	now := at
	var closedAtArg any
	if value := updated.ClosedAtValue(); value != nil {
		closedAtArg = value.Format(time.RFC3339Nano)
	}
	// The close outcome traveled through the state machine into the closed leaf,
	// so the post-transition resolution is read back off the updated issue — no
	// re-attachment, and a non-closed target structurally carries none, so a
	// resolution can never linger on a non-closed row. [LAW:types-are-the-program]
	postResolution := updated.ResolutionValue()
	var resolutionArg any
	if postResolution != nil {
		resolutionArg = string(*postResolution)
	}
	// The redirect target traveled through the state machine into the closed
	// leaf exactly like the resolution — only the redirecting outcomes carry
	// one, structurally — so the plan reads it back off the post-transition
	// issue. Its integrity (target exists, is not the issue itself, is not a
	// deleted canonical) is validated by applyTransitionTx on the write's tx,
	// not here: the plan reads nothing itself, and validateRedirectTarget reads
	// a FOREIGN row. [LAW:effects-at-boundaries] [LAW:one-source-of-truth] The leaf is
	// the single carrier; there is no separate outcome re-extraction to drift
	// from it.
	postRedirect := updated.RedirectTargetValue()
	var redirectTargetArg any
	if postRedirect != nil {
		redirectTargetArg = *postRedirect
	}
	// [LAW:one-source-of-truth] Change rows mirror the columns that actually
	// moved. A same-state reclaim records only the assignee row — a from==to
	// status row would be a schema lie.
	var changes []model.FieldChange
	if fromStatus != toStatus {
		changes = append(changes, model.FieldChange{Field: "status", From: fromStatus, To: toStatus})
	}
	priorClosedAt := issue.ClosedAtValue()
	newClosedAt := updated.ClosedAtValue()
	if !timesEqual(priorClosedAt, newClosedAt) {
		changes = append(changes, model.FieldChange{Field: "closed_at", From: formatNullableTime(priorClosedAt), To: formatNullableTime(newClosedAt)})
	}
	priorResolution := issue.ResolutionValue()
	if !resolutionsEqual(priorResolution, postResolution) {
		changes = append(changes, model.FieldChange{Field: "resolution", From: formatNullableResolution(priorResolution), To: formatNullableResolution(postResolution)})
	}
	priorRedirect := issue.RedirectTargetValue()
	if !stringPointersEqual(priorRedirect, postRedirect) {
		changes = append(changes, model.FieldChange{Field: "redirect_target", From: formatNullableString(priorRedirect), To: formatNullableString(postRedirect)})
	}
	if prior.Assignee != post.Assignee {
		changes = append(changes, model.FieldChange{Field: "assignee", From: prior.Assignee, To: post.Assignee})
	}
	updated.UpdatedAt = now
	// updated already carries the leaf the write will persist — the outcome
	// traveled through the machine, so there is no post-hoc rehydration to
	// re-attach a resolution. [LAW:one-source-of-truth]
	return transitionWrite{
		issueID:           issue.ID,
		toStatus:          toStatus,
		postAssignee:      post.Assignee,
		now:               now,
		closedAtArg:       closedAtArg,
		resolutionArg:     resolutionArg,
		redirectTargetArg: redirectTargetArg,
		action:            action.Name(),
		reason:            reason,
		actor:             actor,
		changes:           changes,
		post:              updated,
		claimHandover:     prior.HandoverTo(post),
	}, nil
}

// applyTransitionTx writes a planned status transition against tx: it validates
// the redirect target, then performs the status UPDATE and the change event.
// The redirect validation lives here rather than in planStatusTransition so the
// plan stays pure: the FOREIGN row it reads (the redirect canonical) is read on
// the tx of the write it guards. Everything else it does is a write, so it still composes into any transaction
// a caller already holds — which is how Apply folds a transition and a field
// write into one commit. [LAW:single-enforcer]
// The redirect target rides the same UPDATE as status/closed_at/resolution, so
// a reopen clears the whole close payload atomically — a stale redirect on a
// live issue is unwritable, not guarded against. [LAW:one-source-of-truth]
func (s *Store) applyTransitionTx(ctx context.Context, tx *sql.Tx, w transitionWrite) error {
	// [LAW:single-enforcer] The redirect target is validated through the one
	// validation site, reading existence+retention of the canonical on this tx.
	// A non-redirecting transition carries no target, so this is a no-op there.
	if err := validateRedirectTarget(ctx, tx, w.post.ID, w.post.ResolutionValue(), w.post.RedirectTargetValue()); err != nil {
		return err
	}
	// [LAW:dataflow-not-control-flow] Status transitions always execute one write.
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET status = ?, assignee = ?, updated_at = ?, closed_at = ?, resolution = ?, redirect_target = ? WHERE id = ?`,
		w.toStatus, w.postAssignee, w.now.Format(time.RFC3339Nano), w.closedAtArg, w.resolutionArg, w.redirectTargetArg, w.issueID); err != nil {
		return fmt.Errorf("update issue status: %w", err)
	}
	return s.recordEvent(ctx, tx, w.issueID, string(w.action), w.reason, w.actor, w.now, w.changes)
}

// retentionWrite is a fully-planned retention transition, the retention-axis
// variant of lifecycleWrite. planRetentionTransition is the compute half: the
// Retain transition table derives the post-state (or the rejection), and the
// change-row diff is taken on the wire projection of the Retention value so
// the history format is the same encoding the columns store. applyTx is the
// write half: one UPDATE plus the event.
type retentionWrite struct {
	issueID string
	now     time.Time
	// nextArchived/nextDeleted are the pair Retain produced, as the
	// retentionColumns projection the columns store.
	nextArchived any
	nextDeleted  any
	action       model.ActionName
	reason       string
	actor        string
	changes      []model.FieldChange
	post         model.Issue
}

func (w retentionWrite) postIssue() model.Issue     { return w.post }
func (w retentionWrite) handover() *claims.Handover { return nil }

// isNoop is always false: the Retain table has no same-state success cell —
// re-archiving an archived issue is a rejection, not a silent no-op — so every
// planned retention transition owes a write.
func (w retentionWrite) isNoop() bool { return false }

// planRetentionTransition plans a retention action against issue as of now. The
// whole retention state machine — legal moves, rejection reasons, the
// delete-on-archived stamp drop — is the pure Retain transition table; this
// plan projects the result into column values.
// [LAW:single-enforcer]
//
// [LAW:effects-at-boundaries] The instant arrives as an argument because this
// is the compute half: it decides nothing about when, so it reads no clock and
// stays testable with no store at all.
func planRetentionTransition(issue model.Issue, actor string, reason string, action model.RetentionAction, now time.Time) (retentionWrite, error) {
	priorArchivedAt, priorDeletedAt := model.RetentionTimestamps(issue.Retention())
	next, err := model.Retain(issue.Retention(), action, now)
	if err != nil {
		return retentionWrite{}, err
	}
	post := issue
	post.SetRetention(next)
	post.UpdatedAt = now
	nextArchivedAt, nextDeletedAt := model.RetentionTimestamps(next)
	nextArchived, nextDeleted := retentionColumns(post)
	var changes []model.FieldChange
	if !timesEqual(priorArchivedAt, nextArchivedAt) {
		changes = append(changes, model.FieldChange{Field: "archived_at", From: formatNullableTime(priorArchivedAt), To: formatNullableTime(nextArchivedAt)})
	}
	if !timesEqual(priorDeletedAt, nextDeletedAt) {
		changes = append(changes, model.FieldChange{Field: "deleted_at", From: formatNullableTime(priorDeletedAt), To: formatNullableTime(nextDeletedAt)})
	}
	return retentionWrite{
		issueID:      issue.ID,
		now:          now,
		nextArchived: nextArchived,
		nextDeleted:  nextDeleted,
		action:       action.Name(),
		reason:       reason,
		actor:        actor,
		changes:      changes,
		post:         post,
	}, nil
}

// applyTx writes a planned retention transition against tx. The UPDATE sets
// only the columns the retention axis owns — updated_at plus the pair — so a
// retention write restates no status column. [LAW:one-source-of-truth]
func (w retentionWrite) applyTx(ctx context.Context, s *Store, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE issues SET updated_at = ?, archived_at = ?, deleted_at = ? WHERE id = ?`,
		w.now.Format(time.RFC3339Nano), w.nextArchived, w.nextDeleted, w.issueID); err != nil {
		return fmt.Errorf("update issue retention: %w", err)
	}
	return s.recordEvent(ctx, tx, w.issueID, string(w.action), w.reason, w.actor, w.now, w.changes)
}

// validateRedirectTarget is the close-validation floor for the redirect
// target the post-transition leaf carries. A NEW redirecting close must carry
// its target (the CLI requires --of; this is the store's integrity floor for
// any other caller — the leaf type alone admits a target-less redirecting
// close because legacy rows genuinely occupy that state). A present target
// must exist and cannot be the issue itself, so the redirect points at a real,
// distinct ticket. It must also not be trash-bound: a redirect to a Deleted
// canonical is a dangling pointer by design, while Archived stays legal —
// "duplicate of something already done" is the most common real redirect
// (decision on links-recut-relations-61nv.1). Deleted is matched as the
// specific variant, not a broader frozen notion, so Archived falls into the
// accept set by the same match.
//
// [LAW:single-enforcer] The one validation site for every close path.
// [LAW:no-ambient-temporal-coupling] The canonical's existence and retention
// are read on the SAME tx that persists the redirect, under the held commit
// lock, so a delete of the canonical is either before this read (rejected
// here) or after the commit. currentRetentionTx returns
// NotFoundError when the row is absent (the existence half) and its retention
// otherwise (the Deleted half), both decoded through the one
// RetentionFromTimestamps boundary.
//
// [LAW:composability] A free function of (ctx, tx, values): it depends on no
// Store state, so it carries no receiver — its signature advertises exactly
// what it reads, matching its siblings currentRetentionTx / requireIssueExistsTx.
func validateRedirectTarget(ctx context.Context, tx *sql.Tx, closingID string, resolution *model.Resolution, target *string) error {
	if target == nil {
		if resolution != nil && resolution.RedirectsToCanonical() {
			return fmt.Errorf("closing as %s requires a canonical target issue to redirect to", *resolution)
		}
		return nil
	}
	if *target == closingID {
		return fmt.Errorf("cannot redirect %s to itself", closingID)
	}
	retention, err := currentRetentionTx(ctx, tx, *target)
	if err != nil {
		return err
	}
	if _, gone := retention.(model.Deleted); gone {
		return fmt.Errorf("cannot redirect %s to %s: the canonical issue is deleted", closingID, *target)
	}
	return nil
}

// currentRetentionTx reads the live retention state of issueID inside tx, for
// validateRedirectTarget's existence and Deleted checks. The pair decodes
// through the single RetentionFromTimestamps boundary like every other read.
func currentRetentionTx(ctx context.Context, tx *sql.Tx, issueID string) (model.Retention, error) {
	var archivedAt, deletedAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT archived_at, deleted_at FROM issues WHERE id = ?`, issueID).Scan(&archivedAt, &deletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, storage.NotFoundError{Entity: "issue", ID: issueID}
		}
		return nil, fmt.Errorf("read issue retention: %w", err)
	}
	archived, err := scanNullableTime(archivedAt)
	if err != nil {
		return nil, err
	}
	deleted, err := scanNullableTime(deletedAt)
	if err != nil {
		return nil, err
	}
	return model.RetentionFromTimestamps(archived, deleted), nil
}

// requireIssueExistsTx verifies issueID names an issue row on tx, returning
// NotFoundError when absent. It is the in-tx endpoint existence check relation
// writes use so the endpoint proven present is the endpoint the edge is written
// against — the read and the insert share one tx under the held commit lock,
// leaving no window between them. [LAW:no-ambient-temporal-coupling]
// Existence is read where the write happens, not earlier. It accepts retained
// (archived/deleted) rows — no deleted_at filter.
func requireIssueExistsTx(ctx context.Context, tx *sql.Tx, issueID string) error {
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM issues WHERE id = ?`, issueID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storage.NotFoundError{Entity: "issue", ID: issueID}
		}
		return fmt.Errorf("check issue exists: %w", err)
	}
	return nil
}

func (s *Store) ListTopics(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT topic FROM issues WHERE deleted_at IS NULL AND topic <> '' ORDER BY topic ASC`)
	if err != nil {
		return nil, fmt.Errorf("list topics: %w", err)
	}
	defer rows.Close()
	topics := []string{}
	for rows.Next() {
		var topic string
		if err := rows.Scan(&topic); err != nil {
			return nil, err
		}
		topics = append(topics, topic)
	}
	return topics, rows.Err()
}

// ListIssueIdentities reads two columns of every row and hydrates nothing, so
// it cannot reach the lifecycle-children walk that ListIssues' hydration takes
// and a parent loop never returns from.
func (s *Store) ListIssueIdentities(ctx context.Context) ([]storage.IssueIdentity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, topic FROM issues ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list issue identities: %w", err)
	}
	defer rows.Close()
	identities := []storage.IssueIdentity{}
	for rows.Next() {
		var identity storage.IssueIdentity
		if err := rows.Scan(&identity.ID, &identity.Topic); err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

// listRelations returns every relation incident to one issue.
//
// The `OR` across two columns is the shape listRelationsForIDs exists to avoid,
// and it is safe HERE for a reason that does not survive generalisation: both
// sides are point predicates on one id, so the disjunct set is two and stays
// two. The landmine is a disjunct set that GROWS with the caller's input — an
// `IN` list on either side of the same `OR` is what turns the engine's overlap
// elimination quadratic. Take this as precedent for a second endpoint column,
// never for a second endpoint LIST. [LAW:carrying-cost]
func (s *Store) listRelations(ctx context.Context, issueID string) ([]model.Relation, error) {
	return queryRelations(ctx, s.db, `relations WHERE src_id = ? OR dst_id = ?`, issueID, issueID)
}
func (s *Store) getMeta(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var row *sql.Row
	if tx != nil {
		row = tx.QueryRowContext(ctx, `SELECT meta_value FROM meta WHERE meta_key = ?`, key)
	} else {
		row = s.db.QueryRowContext(ctx, `SELECT meta_value FROM meta WHERE meta_key = ?`, key)
	}
	var value string
	if err := row.Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get meta %q: %w", key, err)
	}
	return value, nil
}

func (s *Store) setMeta(ctx context.Context, tx *sql.Tx, key, value string) error {
	var execer interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}
	if tx != nil {
		execer = tx
	} else {
		execer = s.db
	}
	if _, err := execer.ExecContext(ctx, `INSERT INTO meta(meta_key, meta_value) VALUES (?, ?)
			ON DUPLICATE KEY UPDATE meta_value = VALUES(meta_value)`, key, value); err != nil {
		return fmt.Errorf("set meta %q: %w", key, err)
	}
	return nil
}

func (s *Store) ensureMetaValue(ctx context.Context, guard *snapshotGuard, key, value string) (bool, error) {
	current, err := s.getMeta(ctx, nil, key)
	if err != nil {
		return false, err
	}
	if current == value {
		return false, nil
	}
	if _, err := guard.ensure(ctx); err != nil {
		return false, fmt.Errorf("ensure meta %s: %w", key, err)
	}
	if err := s.setMeta(ctx, nil, key, value); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ensureMetaDefault(ctx context.Context, guard *snapshotGuard, key, value string) (bool, error) {
	current, err := s.getMeta(ctx, nil, key)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(current) != "" {
		return false, nil
	}
	if _, err := guard.ensure(ctx); err != nil {
		return false, fmt.Errorf("ensure meta %s default: %w", key, err)
	}
	// [LAW:one-source-of-truth] Schema-version writes preserve the recorded version as the canonical migration state once it exists.
	if err := s.setMeta(ctx, nil, key, value); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) listAllLabels(ctx context.Context) ([]model.Label, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT issue_id, label, created_at, created_by FROM labels ORDER BY issue_id ASC, label ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all labels: %w", err)
	}
	defer rows.Close()
	out := []model.Label{}
	for rows.Next() {
		var label model.Label
		var createdAt string
		if err := rows.Scan(&label.IssueID, &label.Name, &createdAt, &label.CreatedBy); err != nil {
			return nil, err
		}
		t, err := scanTime(createdAt)
		if err != nil {
			return nil, err
		}
		label.CreatedAt = t
		out = append(out, label)
	}
	return out, rows.Err()
}

// newEvent stamps one history event for issueID: its id and its attribution,
// read off the store, at the instant its mutation was stamped from the
// history it was planned against (storage.StampTally). Every event the store writes is minted
// here — the row-at-a-time recordEvent, and a reconcile's settled export,
// whose events reach the same tables through applyExportDelta — so two
// writers of history cannot stamp an event two ways. [LAW:single-enforcer]
func (s *Store) newEvent(issueID, action, reason, actor string, at time.Time, changes []model.FieldChange) model.IssueEvent {
	event := model.IssueEvent{
		ID:        "evt-" + uuid.NewString(),
		IssueID:   issueID,
		Action:    strings.TrimSpace(action),
		Reason:    strings.TrimSpace(reason),
		Actor:     storage.Actor(actor),
		CreatedAt: at,
		// The stamp is read off the store rather than passed in, so it is the
		// same for every event this command writes and cannot be forgotten at a
		// call site. Unattributed stores stamp nothing, which is how a store
		// opened for reading and a store from before the feature agree.
		Attribution: s.attribution,
		Changes:     changes,
	}
	return event
}

// recordEvent writes a mutation's history: the event newEvent mints for it,
// through insertEventTx, the one writer of issue_events rows. [LAW:single-enforcer]
// Every mutation site computes its field-change diff and routes through here.
func (s *Store) recordEvent(ctx context.Context, tx *sql.Tx, issueID, action, reason, actor string, at time.Time, changes []model.FieldChange) error {
	return insertEventTx(ctx, tx, s.newEvent(issueID, action, reason, actor, at, changes))
}

// stampEdit is how an edit lands: the event recording the fields it moved, and
// the issue's updated_at at that event's instant. A field edit and a resolved
// reconcile's merged text are both edits, so both take their stamp here and
// cannot disagree about when one happened. [LAW:single-enforcer]
func (s *Store) stampEdit(issue model.Issue, at time.Time, reason, actor string, changes []model.FieldChange) (model.Issue, model.IssueEvent) {
	event := s.newEvent(issue.ID, "", reason, actor, at, changes)
	issue.UpdatedAt = event.CreatedAt
	return issue, event
}

func (s *Store) listComments(ctx context.Context, issueID string) ([]model.Comment, error) {
	return queryComments(ctx, s.db, `comments WHERE issue_id = ?`, issueID)
}

// queryComments reads the comments that from — the FROM clause onward —
// selects, in storage.CommentOrdering. The order is taken in Go, not in SQL: created_at is
// a varchar, and ORDER BY on it sorts a stamp by its spelling. The column list
// lives here beside the Scan that reads it by position, so no caller can name
// the columns in an order the Scan does not expect.
func queryComments(ctx context.Context, q rowQueryer, from string, args ...any) ([]model.Comment, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, issue_id, body, created_at, created_by FROM `+from, args...)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer rows.Close()
	out := []model.Comment{}
	for rows.Next() {
		var c model.Comment
		var createdAt string
		if err := rows.Scan(&c.ID, &c.IssueID, &c.Body, &createdAt, &c.CreatedBy); err != nil {
			return nil, err
		}
		if c.CreatedAt, err = scanTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, storage.CommentOrdering)
	return out, nil
}

// laneFamilyHistory reads issueID's history and its lane family's in one
// statement, so the plan and the stamp come from one snapshot: issueID's own
// events whole, for the plan to decide its holder and phase on, and the rest
// of the family — every issue filed under a parent issueID is filed under,
// the SQL statement of model.LaneFamily — as bare events, since the stamp
// reads only their instants and a sibling's description history is no input
// to it. Every write reads it, so it costs the family's size and not the
// store's: the two halves are a UNION rather than one condition ORed across
// them, and each walks from the issue id it is given to the events filed
// under it by lookup joins. The hints name that walk because Dolt plans every
// unhinted spelling of this read as a scan of issue_events — at 3,000 issues
// outside a 30-issue family, 7ms for the OR and 17ms for the UNION, where
// the hinted read takes 1.2ms, as it does with nothing outside the family. queryEvents parses each stamp into its instant, so the stamp
// never trusts the varchar's spelling.
func (s *Store) laneFamilyHistory(ctx context.Context, issueID string) ([]model.IssueEvent, error) {
	events, err := s.queryEvents(ctx, issueEventsQuery+` UNION ALL `+bareEvents(`JOIN_ORDER(own, sibling, e) LOOKUP_JOIN(own, sibling) LOOKUP_JOIN(sibling, e)`, siblingEdges+`
		JOIN issue_events e ON e.issue_id = sibling.src_id
		WHERE own.src_id = ? AND own.type = ? AND sibling.src_id <> ?`),
		issueID, issueID, string(model.RelParentChild), issueID)
	if err != nil {
		return nil, fmt.Errorf("read lane family history of %s: %w", issueID, err)
	}
	return events, nil
}

// siblingEdges walks from an issue's parent edges, own, to every edge under
// the same parent, sibling: the join that states model.LaneFamily.
const siblingEdges = `relations own
		JOIN relations sibling ON sibling.dst_id = own.dst_id AND sibling.type = own.type`

// LaneFamily reads issueID's family at the family's size. Nothing here reads
// the rank ancestry or lists issues, which is what keeps a start off the
// store's size, and parents are read for their type alone because hydrating
// one hydrates its whole subtree.
func (s *Store) LaneFamily(ctx context.Context, issueID string) (claims.Family, error) {
	events, err := s.laneFamilyHistory(ctx, issueID)
	if err != nil {
		return claims.Family{}, err
	}
	ids, err := s.laneFamilyIDs(ctx, issueID)
	if err != nil {
		return claims.Family{}, err
	}
	named, arg := newIDSet(ids).rows("family")
	parents, err := s.queryParentByChild(ctx, named+` JOIN relations r ON r.src_id = family.id`, arg)
	if err != nil {
		return claims.Family{}, fmt.Errorf("read lane family parents of %s: %w", issueID, err)
	}
	parentTypes, err := s.issueTypesByIDs(ctx, slices.Collect(maps.Values(parents)))
	if err != nil {
		return claims.Family{}, err
	}
	issuesByID, err := s.getIssuesByIDs(ctx, ids)
	if err != nil {
		return claims.Family{}, err
	}
	issues := slices.Collect(maps.Values(issuesByID))
	// No container's key is read: siblings compare on their own, so an empty
	// ancestry is tree order among them, and the full one is the store's size.
	storage.RankAncestry{}.Sort(issues)
	return claims.Family{Issues: issues, ParentByChild: parents, ParentTypes: parentTypes, Events: events}, nil
}

// laneFamilyIDs is the id of every issue in issueID's lane family. It reads
// relations and nothing else, so no issue's retention can reach it.
func (s *Store) laneFamilyIDs(ctx context.Context, issueID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT /*+ JOIN_ORDER(own, sibling) LOOKUP_JOIN(own, sibling) */ sibling.src_id FROM `+siblingEdges+`
		WHERE own.src_id = ? AND own.type = ?`, issueID, string(model.RelParentChild))
	if err != nil {
		return nil, fmt.Errorf("read lane family of %s: %w", issueID, err)
	}
	defer rows.Close()
	ids := []string{issueID}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan lane family of %s: %w", issueID, err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// historyFiledUnder reads the events of every issue filed under parentID as
// bare events, the SQL statement of model.FiledUnder: the history a create
// filed there is stamped past, read only for its instants. No issue is filed
// under the empty parent. Hinted for the reason laneFamilyHistory is.
func (s *Store) historyFiledUnder(ctx context.Context, parentID string) ([]model.IssueEvent, error) {
	events, err := s.queryEvents(ctx, bareEvents(`JOIN_ORDER(r, e) LOOKUP_JOIN(r, e)`, `relations r
		JOIN issue_events e ON e.issue_id = r.src_id
		WHERE r.dst_id = ? AND r.type = ?`), parentID, string(model.RelParentChild))
	if err != nil {
		return nil, fmt.Errorf("read the history filed under %q: %w", parentID, err)
	}
	return events, nil
}

// eventsOf is issueID's own events among a family's history.
func eventsOf(issueID string, family []model.IssueEvent) []model.IssueEvent {
	own := []model.IssueEvent{}
	for _, event := range family {
		if event.IssueID == issueID {
			own = append(own, event)
		}
	}
	return own
}

// StampCorrections is the account of every stamp this store handed out that
// history held back from its clock.
func (s *Store) StampCorrections() storage.StampCorrections {
	return s.stamps.StampCorrections()
}

func (s *Store) ListEvents(ctx context.Context, issueID string) ([]model.IssueEvent, error) {
	events, err := s.queryEvents(ctx, issueEventsQuery, issueID)
	if err != nil {
		return nil, fmt.Errorf("list issue events: %w", err)
	}
	return events, nil
}

// ListAllEvents reads the whole issue history, oldest first. Export uses it to
// serialize the history; claim derivation uses it because a claim is a reading
// of the history and there is no narrower slice that answers the question — the
// establishing event that decides a lane's holder can be arbitrarily old, so a
// recency cutoff would silently drop exactly the claims it was meant to speed up.
func (s *Store) ListAllEvents(ctx context.Context) ([]model.IssueEvent, error) {
	events, err := s.queryEvents(ctx, allEventsQuery)
	if err != nil {
		return nil, fmt.Errorf("list all issue events: %w", err)
	}
	return events, nil
}

// eventColumns is an event as queryEvents reads it, before the change row
// that follows it on each row.
const eventColumns = `e.id, e.issue_id, e.action, e.reason, e.actor, e.created_at, e.stream_id, e.workspace_id`

// allEventsQuery selects every event, one row per change row each, and one
// row of NULL change columns for an event that changed no field.
const allEventsQuery = `SELECT ` + eventColumns + `, c.field, c.from_value, c.to_value
	FROM issue_events e LEFT JOIN issue_event_changes c ON c.event_id = e.id`

// issueEventsQuery selects one issue's events as allEventsQuery does, its id
// the one placeholder. The lookup hint is what makes it read the issue's
// rows: unhinted, Dolt merge-joins the whole of both tables and filters
// after, so one issue's history cost the store's size (4.6ms at 3,000 issues;
// 0.2ms hinted).
const issueEventsQuery = `SELECT /*+ LOOKUP_JOIN(e, c) */ ` + eventColumns + `, c.field, c.from_value, c.to_value
	FROM issue_events e LEFT JOIN issue_event_changes c ON c.event_id = e.id
	WHERE e.issue_id = ?`

// bareEvents selects the events from names, a FROM clause that binds
// issue_events as e, with no change rows: an instant and an attribution, no
// Changes, for a reader that orders by them and decides nothing from what
// they changed. hints is the optimizer hint that walks from to e by lookup.
func bareEvents(hints, from string) string {
	return `SELECT /*+ ` + hints + ` */ ` + eventColumns + `, NULL AS field, NULL AS from_value, NULL AS to_value FROM ` + from
}

// queryEvents runs selection, one of the event statements above or a UNION
// ALL of them, and collapses the per-change rows back
// into IssueEvent.Changes slices. [LAW:dataflow-not-control-flow]
func (s *Store) queryEvents(ctx context.Context, selection string, args ...any) ([]model.IssueEvent, error) {
	q := selection
	// field is in the sort because this is the one place event reads are made
	// deterministic. The export delta compares whole model.IssueEvent values,
	// Changes slice included, so two reads of an event nothing touched have to
	// come back identical or the delta deletes and reinserts it to express no
	// change at all. A join promises no order among the rows sharing an event_id,
	// and issue_event_changes' PRIMARY KEY (event_id, field) is what makes field a
	// TOTAL order over them rather than merely a tighter one. Enforcing it here
	// rather than in the comparison is deliberate: every consumer of this read
	// gets the guarantee, and none of them grows its own. [LAW:single-enforcer]
	//
	// id groups an event's change rows together for the collapse below. It is
	// NOT the event order the contract asks for — created_at is a varchar here,
	// so ordering events in SQL would sort each stamp by its spelling. The
	// events are ordered after hydration instead, where they are instants.
	// The sort names the result's columns, not e's and c's, so it orders a
	// UNION as a whole rather than binding to its last branch.
	q += " ORDER BY id ASC, field ASC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// Ordered list of events; idx maps event ID to its position in out so each
	// change row can be appended to the right event without a second query.
	out := []model.IssueEvent{}
	idx := map[string]int{}
	for rows.Next() {
		var evtID, evtIssueID, evtReason, evtActor, evtCreatedAt string
		var action, evtStream, evtWorkspace, cField, cFrom, cTo sql.NullString
		if err := rows.Scan(&evtID, &evtIssueID, &action, &evtReason, &evtActor, &evtCreatedAt, &evtStream, &evtWorkspace, &cField, &cFrom, &cTo); err != nil {
			return nil, err
		}
		i, seen := idx[evtID]
		if !seen {
			t, err := scanTime(evtCreatedAt)
			if err != nil {
				return nil, err
			}
			event := model.IssueEvent{
				ID:        evtID,
				IssueID:   evtIssueID,
				Reason:    evtReason,
				Actor:     evtActor,
				CreatedAt: t,
				// Routed through NewAttribution so history that predates the
				// feature — and any row a foreign writer left half-filled —
				// arrives as the zero pair rather than as a token with no
				// workspace to scope it. NullString's zero String is "", which
				// is the same absence the constructor already collapses.
				Attribution: model.NewAttribution(evtStream.String, evtWorkspace.String),
				Changes:     []model.FieldChange{},
			}
			if action.Valid {
				event.Action = action.String
			}
			i = len(out)
			idx[evtID] = i
			out = append(out, event)
		}
		if cField.Valid {
			change := model.FieldChange{Field: cField.String}
			if cFrom.Valid {
				change.From = cFrom.String
			}
			if cTo.Valid {
				change.To = cTo.String
			}
			out[i].Changes = append(out[i].Changes, change)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The contract's history order, applied to hydrated values rather than to
	// the column: created_at is stored as RFC3339Nano text, and text comparison
	// puts ".12Z" — three milliseconds EARLIER — after ".123456789Z", because
	// the trimmed encoding runs out at a character where "Z" outranks a digit.
	// A SQL sort on that column therefore disagrees with any engine holding a
	// real instant.
	//
	// [LAW:one-source-of-truth] The comparison is the contract's, not this
	// engine's: storage.EventOrdering is what the memory engine sorts by too,
	// so the rule cannot be edited into disagreement. The sort is stable,
	// so the id grouping above survives it untouched.
	slices.SortStableFunc(out, storage.EventOrdering)
	return out, nil
}

type issueScanner interface{ Scan(dest ...any) error }

type issueRow struct {
	Issue  partialIssue
	Status model.StatusView
}

// rowFields is a row's data as an issue with no lifecycle: every column the
// row holds and its labels, the empty set for none. It is the one conversion
// of row data into an issue. hydrateIssues gives the result its lifecycle;
// undo reads its editable fields on each side of a write through
// storage.IssueFieldValues, and any lifecycle read on it panics (the model's
// own guard), so it never stands in for a hydrated issue. [LAW:one-source-of-truth]
func rowFields(row partialIssue, labels []string) model.Issue {
	issue := model.Issue{
		ID:          row.ID,
		Title:       row.Title,
		Description: row.Description,
		Prompt:      row.Prompt,
		Plan:        row.Plan,
		Priority:    row.Priority,
		IssueType:   row.IssueType,
		Topic:       row.Topic,
		Assignee:    row.Assignee,
		Rank:        row.Rank,
		Lane:        row.Lane,
		Labels:      labels,
		FollowsUp:   row.FollowsUp,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
	issue.SetRetention(row.Retention)
	if issue.Labels == nil {
		issue.Labels = []string{}
	}
	return issue
}

// partialIssue is row data only; hydrateIssues is the only path that may turn
// it into a returned model.Issue.
// [LAW:single-enforcer] Store hydration, not raw row decoding, owns lifecycle construction.
type partialIssue struct {
	ID          string
	Title       string
	Description string
	Prompt      string
	Plan        string
	Priority    model.Priority
	IssueType   model.IssueType
	Topic       string
	Assignee    string
	Rank        string
	Lane        string
	Labels      []string
	FollowsUp   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Retention   model.Retention
}

// nextRankForPlacement resolves a new issue's rank from its requested
// placement and the frame it is being filed into.
//
// The two edges ask different populations, and the asymmetry is the contract
// rather than an oversight. RankBottom asks the whole workspace: it has to
// land the issue after everything that already exists — that is what keeps an
// authored batch in the order its file states — and a rank after every rank is
// also a rank after every frame-mate, so the frame-local reading holds for
// free. Scoping the bottom would seed a first child from an empty frame and
// file it into the middle of the order instead.
//
// RankTop asks the frame. Before everything is not before-my-siblings-only, so
// asking the workspace there would draw the key from a keyspace shared with
// issues the new one is never compared against: a child filed at the top
// would take a midpoint against whichever issue holds the workspace's first
// key, and the next top-level `--top` create would compute its own key
// against that child's.
//
// [LAW:one-source-of-truth] The direction, the population, and the answer for
// a population holding nothing yet all come from the edge — edgeFor resolves
// it, filingBoundsTx reads it, and rankBetweenTx places the key exactly as it
// does for every rank verb — so this function names no end, no query and no
// bound of its own.
func nextRankForPlacement(ctx context.Context, tx *sql.Tx, p storage.RankPlacement, f storage.Frame) (string, error) {
	edge, err := edgeFor(p)
	if err != nil {
		return "", err
	}
	key, err := rankBetweenTx(ctx, tx, func() (string, string, error) {
		return edge.filingBoundsTx(ctx, tx, f)
	})
	if err != nil {
		return "", err
	}
	// The create's INSERT is the one key write writeRanksTx does not report.
	observer := rankObserverFrom(ctx)
	hear(ctx, func() { observer.Wrote(len(key)) })
	return key, nil
}

// issueColumns is the single authoritative ordered projection of the issues
// table. Every read site selects exactly these columns in this order, and the
// positional scanners below (scanIssue, scanIssueWithParent) consume them by
// position — each scanner's Scan() argument order MUST match this slice. A
// column added, removed, or reordered is one edit here that flows to every read
// site through the derived projections, leaving only the two adjacent scanners
// to move with it. [LAW:one-source-of-truth] [FRAMING:representation]
var issueColumns = []string{
	"id", "title", "description", "agent_prompt", "work_plan", "status", "priority",
	"issue_type", "topic", "assignee", "item_rank", "lane", "created_at",
	"updated_at", "closed_at", "resolution", "redirect_target", "follows_up", "archived_at", "deleted_at",
}

// issueProjection renders issueColumns as a SELECT list. A non-empty alias
// qualifies every column (alias "i" yields "i.id, i.title, ..."); the empty
// alias yields the bare form for single-table reads.
func issueProjection(alias string) string {
	qualifier := ""
	if alias != "" {
		qualifier = alias + "."
	}
	cols := make([]string, len(issueColumns))
	for i, c := range issueColumns {
		cols[i] = qualifier + c
	}
	return strings.Join(cols, ", ")
}

// Derived once from issueColumns: the bare form for single-table reads and the
// "i."-qualified form for reads that join the relations table.
var (
	issueColumnsBare      = issueProjection("")
	issueColumnsQualified = issueProjection("i")
)

// scanIssue and scanIssueWithParent consume a row positionally; the Scan()
// argument order below is the other half of issueColumns' positional contract
// and must stay in lockstep with that slice.
func scanIssue(row issueScanner) (issueRow, error) {
	var issue partialIssue
	var prompt sql.NullString
	var status sql.NullString
	var assignee string
	var createdAt, updatedAt string
	var closedAt, resolution, redirectTarget, followsUp, archivedAt, deletedAt sql.NullString
	if err := row.Scan(&issue.ID, &issue.Title, &issue.Description, &prompt, &issue.Plan, &status, &issue.Priority, &issue.IssueType, &issue.Topic, &assignee, &issue.Rank, &issue.Lane, &createdAt, &updatedAt, &closedAt, &resolution, &redirectTarget, &followsUp, &archivedAt, &deletedAt); err != nil {
		return issueRow{}, err
	}
	issue.Prompt = prompt.String
	issue.FollowsUp = followsUp.String
	return parsedIssueRow(issue, status, assignee, createdAt, updatedAt, closedAt, resolution, redirectTarget, archivedAt, deletedAt)
}

func scanIssueWithParent(row issueScanner) (string, issueRow, error) {
	var parentID string
	var issue partialIssue
	var prompt sql.NullString
	var status sql.NullString
	var assignee string
	var createdAt, updatedAt string
	var closedAt, resolution, redirectTarget, followsUp, archivedAt, deletedAt sql.NullString
	if err := row.Scan(&parentID, &issue.ID, &issue.Title, &issue.Description, &prompt, &issue.Plan, &status, &issue.Priority, &issue.IssueType, &issue.Topic, &assignee, &issue.Rank, &issue.Lane, &createdAt, &updatedAt, &closedAt, &resolution, &redirectTarget, &followsUp, &archivedAt, &deletedAt); err != nil {
		return "", issueRow{}, err
	}
	issue.Prompt = prompt.String
	issue.FollowsUp = followsUp.String
	parsed, err := parsedIssueRow(issue, status, assignee, createdAt, updatedAt, closedAt, resolution, redirectTarget, archivedAt, deletedAt)
	return parentID, parsed, err
}

func parsedIssueRow(issue partialIssue, status sql.NullString, assignee string, createdAt string, updatedAt string, closedAt sql.NullString, resolution sql.NullString, redirectTarget sql.NullString, archivedAt sql.NullString, deletedAt sql.NullString) (issueRow, error) {
	var err error
	issue.CreatedAt, err = scanTime(createdAt)
	if err != nil {
		return issueRow{}, err
	}
	issue.UpdatedAt, err = scanTime(updatedAt)
	if err != nil {
		return issueRow{}, err
	}
	// Container rows store NULL status; hydrateIssues ignores StatusView for them
	// and constructs the lifecycle via HydrateAllOf instead.
	// [LAW:single-enforcer] The decision "use StatusView vs derive from children"
	// lives in hydrateIssues; here we just carry the row data as it appears.
	issue.Assignee = assignee
	statusView := model.StatusView{Value: model.State(status.String)}
	if closedAt.Valid {
		t, err := scanTime(closedAt.String)
		if err != nil {
			return issueRow{}, err
		}
		statusView.ClosedAt = &t
	}
	if resolution.Valid {
		// Raw conversion on a read boundary: the bytes were sealed by
		// ParseResolution at write time and the DB CHECK rejects anything else, so
		// this salvage path conserves them without re-parsing. NewStatus drops the
		// resolution for any non-closed state, so a stray value cannot leak onto a
		// non-closed leaf. [LAW:types-are-the-program]
		r := model.Resolution(resolution.String)
		statusView.Resolution = &r
	}
	if redirectTarget.Valid {
		// Same salvage contract as resolution: the value was validated at the
		// write boundary and the DB CHECK pins it to a redirecting resolution;
		// NewStatus drops it for any non-redirecting pairing.
		t := redirectTarget.String
		statusView.RedirectTarget = &t
	}
	var archivedTime, deletedTime *time.Time
	if archivedAt.Valid {
		t, err := scanTime(archivedAt.String)
		if err != nil {
			return issueRow{}, err
		}
		archivedTime = &t
	}
	if deletedAt.Valid {
		t, err := scanTime(deletedAt.String)
		if err != nil {
			return issueRow{}, err
		}
		deletedTime = &t
	}
	issue.Retention = model.RetentionFromTimestamps(archivedTime, deletedTime)
	issue.Labels = []string{}
	return issueRow{Issue: issue, Status: statusView}, nil
}

// scanTime parses an RFC3339Nano timestamp returned by a SQL row scan.
// [LAW:single-enforcer] All RFC3339Nano-typed timestamp columns parse through
// here so the format binding lives in one place; changing the wire format is
// one edit, not 12.
func scanTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

// scanNullableTime parses a nullable RFC3339Nano timestamp column: SQL NULL is
// the absent state (nil), anything else must parse.
func scanNullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	t, err := scanTime(value.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// statusForStorage returns the value to persist in the issues.status column.
// The capability presence is the type-encoded answer to the container-vs-leaf
// question — leaves expose Status, containers do not — so this projection
// never asks IsContainer; it asks the lifecycle.
// [LAW:one-source-of-truth] Container state lives in the AllOf lifecycle, not
// the DB column. Writing NULL keeps the column from lying about what it owns.
func statusForStorage(issue model.Issue) sql.NullString {
	if status := issue.Capabilities().Status; status != nil {
		return sql.NullString{String: string(status.Value), Valid: true}
	}
	return sql.NullString{}
}

func (s *Store) hydrateIssues(ctx context.Context, rows []issueRow) ([]model.Issue, error) {
	return s.hydrateBelow(ctx, rows, make([]*descent, len(rows)))
}

// descent is the chain of containers a hydration descended through to reach a
// row, nearest first; a row the caller asked for has none. It is a list that
// shares its tail, so extending it for a child costs one node.
type descent struct {
	id string
	up *descent
}

// loopTo returns the chain from its nearest container up to id, or nothing
// when id is not on it. A child already on its own chain is a parent cycle:
// the chain from its parent up to it is the loop, each the child of the next,
// and the child of the first.
func (d *descent) loopTo(id string) []string {
	var loop []string
	for at := d; at != nil; at = at.up {
		loop = append(loop, at.id)
		if at.id == id {
			return loop
		}
	}
	return nil
}

// hydrateBelow hydrates rows, each reached through the containers in its
// chain. A container's state is derived from its children's, so a container
// that is its own descendant has none to derive; the descent refuses it there,
// naming the loop, rather than descending into it again. The chain is what
// bounds the descent: no row is hydrated below itself.
// [LAW:no-silent-failure]
func (s *Store) hydrateBelow(ctx context.Context, rows []issueRow, chains []*descent) ([]model.Issue, error) {
	if len(rows) == 0 {
		return []model.Issue{}, nil
	}
	issueIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		issueIDs = append(issueIDs, row.Issue.ID)
	}
	labelsByID, err := s.loadLabelsByIssueIDs(ctx, issueIDs)
	if err != nil {
		return nil, err
	}
	epicIDs := make([]string, 0, len(rows))
	below := make(map[string]*descent, len(rows))
	for i, row := range rows {
		if row.Issue.IssueType.IsContainer() {
			epicIDs = append(epicIDs, row.Issue.ID)
			below[row.Issue.ID] = &descent{id: row.Issue.ID, up: chains[i]}
		}
	}
	childrenByEpicID, err := s.lifecycleChildrenByEpicIDs(ctx, epicIDs, below)
	if err != nil {
		return nil, err
	}
	hydrated := make([]model.Issue, 0, len(rows))
	for _, row := range rows {
		base := rowFields(row.Issue, labelsByID[row.Issue.ID])
		// [LAW:single-enforcer] This store hydrator is the only read boundary
		// that turns row status plus child relations into model lifecycle state.
		// HydrateRow owns the container-vs-leaf dispatch.
		issue, err := model.HydrateRow(base, row.Status, childrenByEpicID[row.Issue.ID])
		if err != nil {
			return nil, err
		}
		// [LAW:single-enforcer] Hydrator post-condition: every returned Issue is
		// fully hydrated. Consumers receive a typed value and never need to
		// re-check IsHydrated() defensively.
		if !issue.IsHydrated() {
			return nil, fmt.Errorf("hydrateIssues: produced unhydrated issue %s", issue.ID)
		}
		hydrated = append(hydrated, issue)
	}
	return hydrated, nil
}

// lifecycleChildrenByEpicIDs hydrates the children of each epic in epicIDs,
// grouped by epic. below holds each epic's chain, the epic itself nearest, and
// every child is hydrated under its epic's.
func (s *Store) lifecycleChildrenByEpicIDs(ctx context.Context, epicIDs []string, below map[string]*descent) (map[string][]model.Issue, error) {
	out := make(map[string][]model.Issue, len(epicIDs))

	// [LAW:dataflow-not-control-flow] Hydrate every epic's children in a single
	// pass rather than once per epic. A parallel parentID slice carries the epic
	// each child row belongs to, so the per-recursion-level query count is one
	// regardless of how many epics are open instead of scaling as one label query
	// plus one child-relation query per epic. hydrateIssues preserves input order
	// and the SELECT groups children by epic (dst_id) then item_rank, so
	// re-bucketing the hydrated result by parentID reproduces the per-epic,
	// rank-ordered grouping.
	childRows, parentIDs, err := s.scanLifecycleChildRows(ctx, newIDSet(epicIDs))
	if err != nil {
		return nil, err
	}
	chains := make([]*descent, len(childRows))
	for i, child := range childRows {
		chains[i] = below[parentIDs[i]]
		if loop := chains[i].loopTo(child.Issue.ID); len(loop) > 0 {
			return nil, storage.ParentCycleError{Cycle: loop}
		}
	}
	hydrated, err := s.hydrateBelow(ctx, childRows, chains)
	if err != nil {
		return nil, err
	}
	for i, issue := range hydrated {
		out[parentIDs[i]] = append(out[parentIDs[i]], issue)
	}
	return out, nil
}

// scanLifecycleChildRows runs the children query and reads its rows, returning
// each child beside the epic it hangs from.
func (s *Store) scanLifecycleChildRows(ctx context.Context, epics idSet) ([]issueRow, []string, error) {
	epicRows, arg := epics.rows("epics")
	// [LAW:one-source-of-truth] Active containers derive progress from active children; archived/deleted containers keep a full child snapshot so their lifecycle state does not collapse to empty/open.
	// Children-of-epic visibility truth table:
	//   parent live, child live -> include
	//   parent live, child dead -> exclude (active container shows only active children)
	//   parent dead, child live -> include (snapshot semantics: container's state at archive)
	//   parent dead, child dead -> include (snapshot semantics)
	// The WHERE clause encodes "include if parent is dead OR child is live."
	rows, err := s.db.QueryContext(ctx, `SELECT r.dst_id, `+issueColumnsQualified+`
		FROM `+epicRows+`
		JOIN relations r ON r.dst_id = epics.id
		JOIN issues i ON i.id = r.src_id
		JOIN issues p ON p.id = r.dst_id
		WHERE r.type = 'parent-child'
			AND (p.archived_at IS NOT NULL OR p.deleted_at IS NOT NULL OR (i.archived_at IS NULL AND i.deleted_at IS NULL))
		ORDER BY r.dst_id ASC, i.item_rank ASC`, arg)
	if err != nil {
		return nil, nil, fmt.Errorf("load lifecycle children: %w", err)
	}
	defer rows.Close()
	childRows := make([]issueRow, 0, len(epics.ids))
	parentIDs := make([]string, 0, len(epics.ids))
	for rows.Next() {
		parentID, child, err := scanIssueWithParent(rows)
		if err != nil {
			return nil, nil, err
		}
		childRows = append(childRows, child)
		parentIDs = append(parentIDs, parentID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return childRows, parentIDs, nil
}

// loadLabelsByIssueIDs reads the labels of every issue in issueIDs, each
// issue's in label order.
func (s *Store) loadLabelsByIssueIDs(ctx context.Context, issueIDs []string) (map[string][]string, error) {
	named, arg := newIDSet(issueIDs).rows("named")
	rows, err := s.db.QueryContext(ctx, `SELECT l.issue_id, l.label FROM `+named+` JOIN labels l ON l.issue_id = named.id ORDER BY l.label ASC`, arg)
	if err != nil {
		return nil, fmt.Errorf("load labels by issue ids: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var issueID, label string
		if err := rows.Scan(&issueID, &label); err != nil {
			return nil, err
		}
		out[issueID] = append(out[issueID], label)
	}
	return out, rows.Err()
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.Format(time.RFC3339Nano)
}

// retentionColumns projects an issue's Retention into the two physical
// archived_at/deleted_at column values. Every SQL statement that writes the
// pair binds this function's outputs, and its input cannot represent
// archived-and-deleted, so the both-set row is unwritable from this codebase.
// [LAW:single-enforcer] The one feeder of the two columns.
func retentionColumns(issue model.Issue) (archivedAt, deletedAt any) {
	a, d := model.RetentionTimestamps(issue.Retention())
	return nullableTime(a), nullableTime(d)
}

// nullableResolution stores a nil resolution as SQL NULL so a closed-without-
// resolution row (a `done` close, legacy, or merged) reads back as the absent
// state, never the empty string.
func nullableResolution(value *model.Resolution) any {
	if value == nil {
		return nil
	}
	return string(*value)
}

// nullableString stores empty strings as SQL NULL so the agent_prompt column reads
// back as "" via sql.NullString.String regardless of whether the row predates
// the column add.
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// formatNullableTime renders a *time.Time for storage in a FieldChange's
// from/to value: nil → "" (SQL NULL via nullableString), non-nil → RFC3339Nano.
func formatNullableTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}

// timesEqual compares two *time.Time pointers by value (both nil is equal,
// nil vs non-nil is not, two non-nil compared with .Equal).
func timesEqual(a, b *time.Time) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Equal(*b)
}

func formatNullableResolution(value *model.Resolution) string {
	if value == nil {
		return ""
	}
	return string(*value)
}

// resolutionsEqual compares two *model.Resolution pointers by value, the same
// nil-aware contract as timesEqual so the event-change diff treats "no
// resolution" symmetrically on both sides.
func resolutionsEqual(a, b *model.Resolution) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// formatNullableString renders an optional string for a FieldChange's from/to
// value: nil → "" (SQL NULL via nullableString), matching formatNullableTime.
func formatNullableString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// stringPointersEqual compares two *string by value, the same nil-aware
// contract as timesEqual/resolutionsEqual.
func stringPointersEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// nullableStringPtr stores a nil optional string as SQL NULL, the pointer
// counterpart of nullableResolution.
func nullableStringPtr(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// ensureDoltDatabase makes the workspace's database exist. A database comes
// into existence in one step: it is built in a staging root beside the Dolt
// root and renamed into place (installDoltRoot), so every reader of "is there
// a database" — this function, OpenForRead's initialized check, LocalHasTickets'
// directory check — sees either none or a complete one, never a database
// another process is still laying down. [LAW:one-source-of-truth] the path's
// existence is the one map of the database's, and the rename keeps it true.
//
// The bootstrap lock owns the decision to build: its holder alone answers
// "absent, so I build", so two first opens racing on a fresh workspace build
// once — the second waits, then finds the first's database. The answer is
// read under the lock on every call, not before it, because a check made
// before the wait is a decision about a database another process may be
// installing. [LAW:no-ambient-temporal-coupling] An existing database needs
// nothing more: branch normalization runs on each write open's own engine
// (Open/OpenSync call ensureMasterDefaultBranch), which also covers databases
// that arrive pre-made, like an adopt's clone, so the two bootstrap engines
// are paid for only by the call that builds. [LAW:carrying-cost]
//
// Reports what this call did to make the database exist.
func ensureDoltDatabase(ctx context.Context, stderr io.Writer, doltRootDir string, workspaceID string) (_ storage.Bootstrap, err error) {
	root := filepath.Clean(doltRootDir)
	release, err := acquireBootstrapLock(ctx, stderr, root)
	if err != nil {
		return storage.BootstrapNone, err
	}
	defer func() {
		if relErr := release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	if dirExists(filepath.Join(root, doltDatabaseName, ".dolt")) {
		return storage.BootstrapNone, nil
	}
	staging := bootstrapStagingPath(root)
	bootstrap, err := discardDeadBuilds(staging, filepath.Join(root, doltDatabaseName))
	if err != nil {
		return storage.BootstrapNone, err
	}
	if err := buildDoltRoot(ctx, stderr, staging, workspaceID); err != nil {
		return storage.BootstrapNone, errors.Join(err, os.RemoveAll(staging))
	}
	if err := installDoltRoot(staging, root); err != nil {
		return storage.BootstrapNone, errors.Join(err, os.RemoveAll(staging))
	}
	return bootstrap, nil
}

// discardDeadBuilds clears the paths a build writes of what a builder that
// died mid-build left there: the staging root, and a database directory with
// no .dolt in it, which is what a lit from before staged builds left when it
// died laying a database down in place. The caller holds the bootstrap lock
// and found no database, so no builder that takes the lock owns either path,
// and with both cleared the build and its install write onto nothing. The
// "is there a database" check and the install's rename then agree on what
// the database is. Reports whether there was anything to discard.
func discardDeadBuilds(paths ...string) (storage.Bootstrap, error) {
	bootstrap := storage.BootstrapBuilt
	for _, path := range paths {
		switch _, err := os.Lstat(path); {
		case err == nil:
			bootstrap = storage.BootstrapBuiltOverDeadBuild
			if err := os.RemoveAll(path); err != nil {
				return storage.BootstrapNone, fmt.Errorf("remove a dead build's leftover: %w", err)
			}
		case !errors.Is(err, os.ErrNotExist):
			return storage.BootstrapNone, fmt.Errorf("stat a build's path for a dead build's leftover: %w", err)
		}
	}
	return bootstrap, nil
}

// buildDoltRoot lays a fresh Dolt root down at staging, which the caller has
// cleared: the database created and its default branch named master.
func buildDoltRoot(ctx context.Context, stderr io.Writer, staging string, workspaceID string) error {
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("create bootstrap staging root: %w", err)
	}
	// The two bootstrap pools run strictly one-after-another: with the singleton
	// cache bypassed, each pool owns a real engine whose journal lock lives until
	// that pool closes, so the second open must not begin while the first is
	// still live. [LAW:no-ambient-temporal-coupling] The explicit Close IS the
	// ordering owner — a deferred close would hold both engines at once.
	if err := func() error {
		db, hold, err := openHeldPool(ctx, stderr, staging, workspaceID, "", engineWrite, &longPhase{})
		if err != nil {
			return err
		}
		defer hold.end()
		defer db.Close()
		// A pool with no database takes no LOCK at its ping: the database it
		// creates takes LOCK here, so this runs under the hold as well.
		if _, err := db.ExecContext(hold.under(ctx), fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", doltDatabaseName)); err != nil {
			// So a foreign journal-lock holder surfaces here, not at the
			// ping; the wrapper names it, and passes every other failure
			// through untouched.
			return wrapEngineOpenContention(fmt.Errorf("create dolt database: %w", err), staging)
		}
		return nil
	}(); err != nil {
		return err
	}
	db, hold, err := openHeldPool(ctx, stderr, staging, workspaceID, doltDatabaseName, engineWrite, &longPhase{})
	if err != nil {
		return err
	}
	defer hold.end()
	defer db.Close()
	return ensureMasterDefaultBranch(ctx, db)
}

// installDoltRoot moves a built database into place with one rename, onto
// the path its readers key on. OpenForRead keys on the Dolt root itself, so
// with no root yet the whole staged root is renamed in and the root arrives
// with its database inside. A root that is already there (the caller checked
// it holds no database) keeps lit's own files beside the database, such as
// an adopt's marker, so only the database directory is renamed into it and
// those files stay where they are. The staging root, emptied by the second
// kind of install, is then removed.
//
// A database built here has received nothing from the remote, so a
// received-refs record standing beside it describes a database that is gone,
// and is forgotten before the new one appears: a record may lag the database
// beside it but never lead it (received_refs.go). [LAW:one-source-of-truth]
func installDoltRoot(staging, root string) error {
	if err := forgetReceivedRefs(root); err != nil {
		return err
	}
	from, to := staging, root
	if dirExists(root) {
		from, to = filepath.Join(staging, doltDatabaseName), filepath.Join(root, doltDatabaseName)
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("install bootstrapped database: %w", err)
	}
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("remove emptied bootstrap staging root: %w", err)
	}
	return nil
}

// masterRenameSource is the lock-free half of branch normalization: it reads
// the branch state and names the branch a rename must move, or "" when the
// store is already normalized (master present, or a multi-branch shape the
// rename must not touch). Callers that must not block on the commit lock for
// a no-op — OpenSync, whose read-only consumers would otherwise queue behind
// a snapshot copy's minutes-long hold — consult this first and take the lock
// only on a non-empty answer; the rename itself re-derives under the lock.
func masterRenameSource(ctx context.Context, db *sql.DB) (string, error) {
	activeBranch := ""
	if err := db.QueryRowContext(ctx, `SELECT active_branch()`).Scan(&activeBranch); err != nil {
		return "", fmt.Errorf("query dolt active branch: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM dolt_branches ORDER BY name`)
	if err != nil {
		return "", fmt.Errorf("query dolt branches: %w", err)
	}
	defer rows.Close()
	hasMaster := false
	branchCount := 0
	for rows.Next() {
		var branchName string
		if err := rows.Scan(&branchName); err != nil {
			return "", fmt.Errorf("scan dolt branch: %w", err)
		}
		hasMaster = hasMaster || branchName == "master"
		branchCount++
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate dolt branches: %w", err)
	}
	if activeBranch == "master" || hasMaster || branchCount != 1 {
		return "", nil
	}
	return activeBranch, nil
}

func ensureMasterDefaultBranch(ctx context.Context, db *sql.DB) error {
	activeBranch, err := masterRenameSource(ctx, db)
	if err != nil || activeBranch == "" {
		return err
	}
	// [LAW:one-source-of-truth] Embedded bootstrap normalizes the initial Dolt branch name at database creation time so callers do not re-encode branch-policy drift.
	renameQuery := fmt.Sprintf(
		"CALL DOLT_BRANCH('-m', '%s', 'master')",
		strings.ReplaceAll(activeBranch, "'", "''"),
	)
	if _, err := db.ExecContext(ctx, renameQuery); err != nil {
		return fmt.Errorf("rename dolt default branch to master: %w", err)
	}
	return nil
}

// The co-resident-holder sizing chain. Measured facts at the root — what each
// routine holder of the live store costs to wait out — and every wait in the
// store derived from them by arithmetic. [LAW:one-source-of-truth] Hand-set
// waits would each have to be remembered into agreement, and one set below the
// cost of the work it bounds is nothing the code could notice. Change a
// measurement; the waits follow.
//
// The mirror holds the live store only to clone it — workspace shared,
// Dolt's LOCK, commit lock, one copy-on-write clone of the dolt directory,
// release — and pushes from the clone with no lock on the live store held.
// The push's own deadline is the separate chain below (mirrorPushDeadline).
const (
	// mirrorCloneObservedTail is the slowest a HEALTHY hold — take the three
	// locks, clone the dolt directory, clear the pending marker, release —
	// has been measured to run. Measured 2026-09-27 against this repository's
	// own store (283 MB on disk: 94 MB noms, 183 MB git-remote-cache), load
	// average 7–13, as the clone step alone, which is the whole hold but for
	// three flock calls and one unlink:
	//
	//   - APFS clonefile of the whole directory (dbsnapshot.CloneTree's
	//     Darwin path), 6 samples: 0.109s–0.240s.
	//   - byte-for-byte copy of the same directory (the walk-and-copy path a
	//     filesystem without copy-on-write falls back to), 2 samples:
	//     0.390s, 0.461s.
	//
	// The hold has two healthy regimes — a copy-on-write clone whose cost is
	// metadata, and a byte copy whose cost is proportional to the store — and
	// a stall detector sits above the SLOWER of them, so the figure is the
	// byte copy's tail rounded up to the next 100ms, not the clone's.
	// [FRAMING:representation] a map of an operation whose territory grows
	// with the store; mirror.log's `hold=` values are the place to re-measure
	// from, and a byte-copy regime on a slow disk is the reading that would
	// move it.
	mirrorCloneObservedTail = 500 * time.Millisecond

	// mirrorHoldStallFactor is what separates "slow" from "stalled". A hold
	// still running at twice the slowest healthy hold ever recorded is not a
	// hold running a bit long; it is a copy that has stopped making progress,
	// and the budget fires there. A budget set AT the cost of the work is not
	// a safety valve; it is a scheduled failure.
	mirrorHoldStallFactor = 2

	// mirrorHoldBudget is the deadline each mirror hold on the live store runs
	// under — the clone step, and the pushed-head record after the push. The
	// exported MirrorHoldBudget below is the variable the cli reads and tests
	// shrink; this const is what it starts at, so the waits derived from it
	// further down stay const arithmetic.
	mirrorHoldBudget = mirrorCloneObservedTail * mirrorHoldStallFactor

	// mirrorHoldCancelLag is how much longer a cut hold can keep the store
	// after its deadline has fired. The clone step's one uncancelable unit is
	// a single clonefile syscall (the walk-and-copy path checks the context
	// between 32 MiB chunks), and a syscall that ran long is bounded by the
	// clone tail above; the record step's is one journal append. So a cut
	// lands within one observed tail of its deadline — a bound derived from
	// the measurement, not a second measurement, which is why it is not
	// exported as an "observed" figure.
	mirrorHoldCancelLag = mirrorCloneObservedTail

	// mirrorHoldCeiling is the longest a mirror can hold the live store's
	// LOCK and commit lock: its deadline plus the lag its cut takes to land.
	// This — not the budget — is the number every co-resident waiter must
	// outlast.
	mirrorHoldCeiling = mirrorHoldBudget + mirrorHoldCancelLag

	// storeLockPollInterval is how often a contender re-tries a held lock,
	// and so the granularity of every wait in this package: a holder that
	// releases is noticed within this much. coResidentWaitHeadroom is a
	// multiple of it for that reason — the headroom has to cover sleeping
	// through the release, not a step of the hold. [LAW:one-source-of-truth]
	// declared once, read by holdWait and by the sizing chain here.
	storeLockPollInterval = 100 * time.Millisecond

	// coResidentWaitHeadroom is scheduling slop above the ceiling, and the one
	// number here that is a judgment rather than a measurement — so it is
	// pinned to something real: eight polls, which is how much of the wait
	// can be spent asleep between polls on a machine under load. It is
	// deliberately not folded into the steps above; slop hidden inside a
	// measured step is how a measured step stops being measurable.
	coResidentWaitHeadroom = 8 * storeLockPollInterval

	// ReceiveDeadline bounds the automatic receive — the remote check and
	// the fetch its detached worker runs when the receive debounce lapses —
	// so an offline or slow remote cannot keep the worker running
	// indefinitely. A cut abandons only the fetch (the next interval
	// retries). The fetch runs on a clone of this store, so this deadline
	// bounds the worker, not a hold: the live store is held for the clone's
	// copy (under MirrorHoldBudget) and for the landing of the fetch, whose
	// cost is proportional to how far the remote moved and which only this
	// deadline bounds (LandFetchedHead says why). [LAW:one-source-of-truth]
	ReceiveDeadline = 15 * time.Second
)

// coResidentHolderWait is the ONE answer to "how long does a caller wait for
// a co-resident holder of this store to let go" — a live write Store in this
// or another process, a non-lit dolt process, or the snapshot copy's
// LockDoltJournalExclusive hold. It is sized to the longest ROUTINE hold, the
// mirror's clone or record step, because a wait shorter than a legal routine
// hold does not protect anyone, it manufactures "another process is holding
// this workspace's Dolt store open" out of a workspace behaving exactly as
// designed — and a wait longer than that answers a stalled holder with
// silence, which is what sends a user or agent into a retry that adds a
// second waiter. Every other holder is not routine and is named: a snapshot
// copy, a migration, an import, the explicit push (which still holds the
// store across the network), all of them fail a contender after this wait
// with the holder's pid, command and age in the refusal. The automatic
// receive fetches on a clone, so its holds on this store — the copy, the
// landing, the settle — are routine ones, except a landing of a fetch that
// brought much more than the usual few commits, which is named like the rest.
//
// The wait counts from the last time the holders in front of the contender
// showed progress, not from the contender's arrival (holdWait): sixteen `lit
// new` at once serialize on this store at tens of milliseconds each and every
// one lands, because each one's arrival is progress; a holder that stands
// still for this long is the one that fails the contender. A routine write
// hold (engineWriteRoutine) also shows progress while its engine works, by
// heartbeat (lockHold), so it is waited out however long a loaded machine
// stretches it; this figure is then the longest it may go without engine
// work. Every hold that sends no heartbeat (the long holds above, a foreign
// dolt, a lit older than heartbeats) is judged by this figure exactly as
// before. A package variable so
// tests can set it: contention tests shrink it rather than sleep through the
// production one, and a test whose subject is not the budget lifts it out of
// play. Nothing else assigns it.
var coResidentHolderWait = mirrorHoldCeiling + coResidentWaitHeadroom

// The push deadline chain. The mirror's push runs from a clone with no lock on
// the live store held, so nothing waits on it — but the mirror process itself
// holds the single-flight sync-push lock for its whole run, and every mirror
// spawned meanwhile loses that race and exits. A push hung on a stalled
// transport would therefore stop pushes for as long as the remote cared to
// stall, so the push runs under a deadline.
const (
	// mirrorPushObservedTail is the slowest a HEALTHY push — open the clone's
	// engine, push, close — has been measured to run on a real workspace.
	// Measured 2026-09-12 against this repo's own store, from two samples,
	// because the one the mirror keeps for itself is censored by the very
	// deadline sized from it:
	//
	//   - .git/links/mirror.log, 279 cycles: min 10.6s; p50 13.2s over all of
	//     them, 12.7s over the 235 that were not cut; slowest uncut cycle
	//     20.0s. 44 of the 279 (15.8%) were cut, so every cycle that would
	//     have run longer is recorded as a cut and the log cannot show the
	//     tail. A floor, never a ceiling.
	//   - 20 foreground `lit sync push` runs of the same store: min 9.8s,
	//     p50 12.2s, max 17.0s. Uncensored — the foreground push shares
	//     performSyncPush with the mirror and is deliberately unbounded — so
	//     this is the sample the deadline can honestly be sized against.
	//
	// Compared like with like — completed work against completed work, the
	// mirror's uncut p50 of 12.7s against the foreground's 12.2s — the two
	// agree to about half a second: the background path does not cost twice
	// the foreground one. Both paths ARE the push. The engine open and close
	// bracketing it measure ~0.3s together, so the cost is the network round
	// trip and nothing else. [FRAMING:representation] a map of an operation
	// whose territory — a push against a remote holding a repository that
	// grows — moves; mirror.log's `push=` values are the place to re-measure
	// from.
	mirrorPushObservedTail = 20 * time.Second

	// mirrorPushStallFactor: a transport that has stopped answering is not a
	// push running a bit long, it is a push that will never end, so the
	// deadline fires at twice the slowest healthy push ever recorded, where
	// the only thing on the far side is a stall.
	mirrorPushStallFactor = 2

	// mirrorPushDeadline is the deadline the mirror's push runs under. The
	// exported MirrorPushDeadline below is the variable the cli reads and the
	// deadline regression test shrinks; this const is what it starts at.
	mirrorPushDeadline = mirrorPushObservedTail * mirrorPushStallFactor

	// MirrorPushCancelLagObserved is how much longer a cut push keeps its
	// engine (the clone's) after its deadline has already fired:
	// cancellation reaches the transport, but the push does not unwind
	// instantly. Measured 2026-09-12 over the 44 cut cycles in mirror.log as
	// elapsed-minus-budget: p50 1.3s, but 21.4s at the tail.
	//
	// Exported because the cli's push-deadline regression tests assert where
	// a cut push ENDS, and restating that bound there would be a second,
	// unattributed copy of this figure that a re-measurement here leaves
	// behind. [LAW:one-source-of-truth] No store wait is sized against it: the
	// push holds nothing on the live store, so its lag is the mirror process's
	// own lifetime and nobody else's.
	MirrorPushCancelLagObserved = 22 * time.Second
)

// MirrorHoldBudget is the deadline each background-mirror hold on the live
// store runs under: the clone step before the push, and the pushed-head record
// after it. It is a deadline, not a bound on the hold: a cut lands up to
// mirrorHoldCancelLag later, and mirrorHoldCeiling is the number that follows
// from that. TestMirrorHoldBudgetExceedsObservedCloneCost and
// TestCoResidentWaitIsSizedToTheMirrorHold pin both relations. A package
// variable so a regression test can shrink it without sleeping through the
// production one.
var MirrorHoldBudget = mirrorHoldBudget

// MirrorPushDeadline is the deadline the background mirror's push runs
// under: the clone engine's open and the push from it. The close after them
// takes no ctx and is not bounded by it. The push traverses
// the network with no inherent bound, so the bound is imposed by the actor
// that owns it. It holds nothing on the live store; it exists so a stalled
// transport cannot wedge the single-flight mirror forever.
// TestMirrorPushDeadlineExceedsObservedPushCost pins its relation to the
// measured push. A package variable so the deadline regression test can
// shrink it without sleeping through the production one.
var MirrorPushDeadline = mirrorPushDeadline

// HistoryBoundDeadline is the deadline a landed push's history bound runs
// under. The bound crosses the network three times — the re-root's lease
// push, the remote's GC, the fetch that learns what the remote carries — and
// each leg is a transport of a push's kind, so each gets the push deadline.
// The local legs, the re-roots, ride in the stall factor's margin; their cost
// is reroot.elapsed_ms. The bound's follow collects nothing below the cut:
// the live store's push verb runs that full GC after the bound, on its own
// lifetime (collectOwedCut). The remote GC's upload has not been measured
// against a hosted remote; history_bound.elapsed_ms on the push's event is
// the figure to re-measure from. A cut bound fails nothing:
// the push landed. A cut before the re-root reaches the remote leaves the
// next push past the bound to run it again; a cut after it leaves the
// remote's history below the cut to the next re-root's GC, which collects it
// with its own, since the remote's GC replaces nothing once a push lands on
// top of the re-root. A function because the push deadline it derives from
// is a variable. [LAW:one-source-of-truth]
func HistoryBoundDeadline() time.Duration { return 3 * MirrorPushDeadline }

// wrapEngineOpenContention attaches operator guidance to an engine open that
// exhausted its wait against a held Dolt journal lock, naming who holds it;
// every other error passes through untouched. The wrap carries BOTH
// discriminators: ErrWorkspaceBusy, the one contention sentinel every store
// lock stamps — which is what keeps the mirror's push outcome recording a
// busy workspace as workspace_busy rather than a failure that pages the
// owner — and the underlying nbs.ErrDatabaseLocked classification.
// [LAW:single-enforcer] The one place the raw "database is locked" outcome
// becomes a holder-naming, actionable message. It takes the dolt root and
// not a prepared account so the account is read at the moment of failure,
// naming the holder that is there now rather than the one that was there
// when the wait began.
func wrapEngineOpenContention(err error, doltRootDir string) error {
	if err != nil && errors.Is(err, nbs.ErrDatabaseLocked) {
		return fmt.Errorf("another process is holding this workspace's Dolt store open, %s; retry after it completes: %w (%w)",
			describeLockHolders(workspaceStorageDir(doltRootDir), DoltJournalLockPath(doltRootDir)), ErrWorkspaceBusy, err)
	}
	return err
}

// coResidentHolderWaitNow reads the wait at the moment a contender needs it,
// so a connector built before a contention test shrank the variable still
// waits the shrunken figure. [LAW:one-source-of-truth]
func coResidentHolderWaitNow() time.Duration { return coResidentHolderWait }

// newDoltConnector assembles the embedded-driver connector every store
// connection is built from. database selects the initial database ("" for the
// bootstrap opens that create it). access is the open contract: engineWrite
// opens fail fast on a foreign journal-lock holder and retry (bounded) instead
// of accepting dolt's silent read-only fallback — a "successfully opened" write
// store IS the proof it can write ([LAW:parse-dont-validate]); engineRead opens
// read-only without trying the lock, because reading beside a live writer is a
// read open's whole contract and a read has no business taking the lock that
// keeps writers out, or the repairs and flushes that come with holding it.
// Both bypass dolt's in-process singleton cache, so a Store's
// lifetime and its engine's (and journal lock's) lifetime are the same fact
// rather than a cached shadow that outlives Close. [LAW:one-source-of-truth]
func newDoltConnector(doltRootDir, workspaceID, database string, hold *lockHold) (*embedded.Connector, error) {
	author := strings.TrimSpace(workspaceID)
	if author == "" {
		author = "links"
	}
	author = strings.ReplaceAll(author, "@", "_")
	cfg := embedded.Config{
		Directory:             filepath.Clean(doltRootDir),
		CommitName:            author,
		CommitEmail:           fmt.Sprintf("%s@links.local", author),
		Database:              database,
		DisableSingletonCache: true,
	}
	switch hold.access {
	case engineRead:
		cfg.JournalReadOnly = true
	case engineWrite, engineWriteRoutine:
		// The hold owns the open's wait, so the one value that waited is the
		// one that reports how the wait went (lockHold.reportWait).
		cfg.BackOff = hold.wait
	default:
		return nil, fmt.Errorf("open dolt: unknown engine access %d", hold.access)
	}
	connector, err := embedded.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("open dolt: %w", err)
	}
	return connector, nil
}

// openDoltPool wraps newDoltConnector's connector in the one configured
// database/sql pool shape every store connection uses.
func openDoltPool(doltRootDir, workspaceID, database string, hold *lockHold) (*sql.DB, error) {
	connector, err := newDoltConnector(doltRootDir, workspaceID, database, hold)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(executingConnector{inner: connector, hold: hold})
	// [LAW:single-enforcer] Each Store owns one embedded Dolt SQL connection so the process cannot self-conflict through the database/sql pool.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	return db, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
