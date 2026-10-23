package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

const migrationCheckpointPrefix = "pre-migrate"
const migrationCheckpointRetention = 5

// migrationDriftRepairCheckpointPrefix names the Dolt checkpoint branch taken
// immediately before a version-content drift repair (repairVersionContentDriftWithRollback).
// Distinct from migrationCheckpointPrefix's ordinary per-migration checkpoints
// so pruning the two independently retained sets never collides.
const migrationDriftRepairCheckpointPrefix = "pre-drift-repair"

// migrationUpByOneForTest, if non-nil, replaces the goose step inside
// applyPendingMigrations ONLY — the durable startup path. Tests use it to inject
// migration failures without needing real failing migrations in the embedded
// registry. The transient reconcile schema-lift (liftWorkingSetToRegistry) steps
// through the bare upByOne and does NOT consult this hook, so a startup-failure
// test can never poison the reconcile lift. [LAW:composability] the hook couples
// only to the caller that needs it, not to every consumer of the shared stepper.
// It receives the connection the step runs on, so a test can leave the partial
// changes a real step's statements leave before it fails.
var migrationUpByOneForTest func(ctx context.Context, db *sql.DB, provider *goose.Provider) (*goose.MigrationResult, error)

// versionContentDriftRepairForTest, if non-nil, replaces the repair inside
// repairVersionContentDriftWithRollback, so a test can fail the repair after
// its checkpoint exists.
var versionContentDriftRepairForTest func(ctx context.Context) ([]string, error)

// MigrationStepError is a goose step of the migration run, or its commit,
// that failed: the migration goose reported, zero values when it reported
// none, and why the step failed. It is the failed migration's one
// representation; the quarantine the failure earns is built from it.
type MigrationStepError struct {
	Version int64
	Name    string
	Cause   error
}

func (e *MigrationStepError) Error() string {
	if e.Version == 0 {
		return fmt.Sprintf("migration (version unknown) failed: %v", e.Cause)
	}
	return fmt.Sprintf("migration v%d %q failed: %v", e.Version, e.Name, e.Cause)
}

func (e *MigrationStepError) Unwrap() error { return e.Cause }

// quarantine is the quarantine this failure earns: the row recordQuarantine
// writes and the value the failure reports in force are this one value.
func (e *MigrationStepError) quarantine() *QuarantineBlockError {
	return &QuarantineBlockError{Version: e.Version, Name: e.Name, ErrorText: e.Cause.Error()}
}

// EventRecord is the version goose reported for the failed step, 0 when it
// reported none.
func (e *MigrationStepError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{"migration.failed_version": e.Version}, nil
}

// CheckpointResetError is work that ran under a Dolt checkpoint (the
// migration run, or a version-content drift repair) and failed, after which
// the working set was reset to the checkpoint. The message names the
// checkpoint as a forensics anchor beside the recovery snapshot the open's
// SchemaConvergenceError names.
//
// [LAW:single-enforcer] The recovery-instruction format lives here so every
// caller sees the same operator-facing words.
// [LAW:types-are-the-program] A reset that failed is CheckpointResetFailedError:
// what the reset left is the type, not a flag on it.
type CheckpointResetError struct {
	Checkpoint storage.Checkpoint
	// Cause is why the work failed.
	Cause error
	// Quarantine is the quarantine in force after this failure, nil when none
	// is. It is in force once its row is in the working set, committed or not:
	// an uncommitted row is what the next open reads.
	Quarantine *QuarantineBlockError
	// QuarantineErr is why recording the quarantine the failure earned did not
	// complete, nil when it completed or none was earned. Every pairing with
	// Quarantine is a real state: a failed insert leaves none in force, a
	// failed commit leaves one.
	QuarantineErr error
}

// Error states the reset, and then what the next open meets: the quarantine
// in force and how to clear it, or the work run again. Whether a quarantine is
// in force is said only where one was earned; a drift repair never earns one.
func (e *CheckpointResetError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%v\n\nthe working set was automatically reset to Dolt checkpoint %q", e.Cause, e.Checkpoint.Name)
	if e.QuarantineErr != nil {
		fmt.Fprintf(&b, "\nrecording its quarantine failed: %v", e.QuarantineErr)
	}
	switch {
	case e.Quarantine != nil:
		b.WriteString("\nthe quarantine is in force; to retry after fixing the migration, clear it:\n  lit unquarantine")
	case e.QuarantineErr != nil:
		b.WriteString("\nno quarantine is in force, so the next open runs it again")
	default:
		b.WriteString("\nthe next open runs it again")
	}
	return b.String()
}

// Unwrap exposes the cause, why recording the quarantine failed, and the
// quarantine in force: from this run on the workspace is quarantined, so the
// failure carries the same *QuarantineBlockError every later open refuses
// with, and a caller classifying it sees one state, not two.
// [LAW:one-source-of-truth]
func (e *CheckpointResetError) Unwrap() []error {
	errs := []error{e.Cause}
	if e.QuarantineErr != nil {
		errs = append(errs, e.QuarantineErr)
	}
	if e.Quarantine != nil {
		errs = append(errs, e.Quarantine)
	}
	return errs
}

// EventRecord is the checkpoint the working set was reset to, and why
// recording the quarantine failed as the recovery error.
func (e *CheckpointResetError) EventRecord() (map[string]any, map[string]int) {
	return checkpointRecoveryFacts(e.Checkpoint, e.QuarantineErr), nil
}

// CheckpointResetFailedError is work that ran under a Dolt checkpoint and
// failed, after which resetting the working set to the checkpoint failed too.
// The working set may still hold the work's changes; what that calls for is
// the failed step's act, so the message only says what is true.
type CheckpointResetFailedError struct {
	Checkpoint storage.Checkpoint
	// Cause is why the work failed.
	Cause error
	// ResetErr is why the reset failed.
	ResetErr error
}

func (e *CheckpointResetFailedError) Error() string {
	return fmt.Sprintf(
		"%v\n\nresetting the working set to Dolt checkpoint %q failed too: %v\nthe working set may still hold the failed work's changes",
		e.Cause, e.Checkpoint.Name, e.ResetErr,
	)
}

func (e *CheckpointResetFailedError) Unwrap() []error { return []error{e.ResetErr, e.Cause} }

// EventRecord is the checkpoint the reset tried to reach, and why it failed
// as the recovery error.
func (e *CheckpointResetFailedError) EventRecord() (map[string]any, map[string]int) {
	return checkpointRecoveryFacts(e.Checkpoint, e.ResetErr), nil
}

// checkpointRecoveryFacts is what a checkpointed work's recovery left, under
// the keys both reset outcomes record: the checkpoint, and why the recovery
// step the failure names failed, null when the recovery held. One key for
// every recovery step: migration.failed_step already says which.
// [LAW:one-source-of-truth]
func checkpointRecoveryFacts(checkpoint storage.Checkpoint, recoveryErr error) map[string]any {
	return map[string]any{
		"migration.checkpoint":     checkpoint.Name,
		"migration.recovery_error": telemetry.ErrorText(recoveryErr),
	}
}

var (
	_ model.RecordedError = (*MigrationStepError)(nil)
	_ model.RecordedError = (*CheckpointResetError)(nil)
	_ model.RecordedError = (*CheckpointResetFailedError)(nil)
	_ model.RecordedError = (*QuarantineBlockError)(nil)
)

// QuarantineBlockError is returned when Open finds a pending migration version
// that has a quarantine record from a previous failed attempt.
//
// [LAW:single-enforcer] The quarantine-error format lives here so operator
// tooling and tests have one place to parse the recovery instructions.
type QuarantineBlockError struct {
	Version   int64
	Name      string
	ErrorText string // original failure message recorded at quarantine time
	// Next is the quarantine the open is refused on once this one is cleared,
	// nil when none. Clearing a quarantine changes neither the applied version
	// nor the phase, so it is what the gate refuses on after the clear.
	Next *QuarantinedMigration
}

// QuarantinedMigration names a migration whose quarantine is in force.
type QuarantinedMigration struct {
	Version int64
	Name    string
}

func (e *QuarantineBlockError) Error() string {
	return fmt.Sprintf(
		"migration v%d %q is quarantined after a previous failure:\n  %s\n\n"+
			"to recover, either:\n"+
			"  (a) restore from a dbsnapshot: lit snapshots restore <name>\n"+
			"  (b) clear the quarantine (if transient): lit unquarantine",
		e.Version, e.Name, e.ErrorText,
	)
}

// EventRecord is the version whose quarantine is in force. Every failure that
// meets a quarantine carries this type, so every one of them records it: the
// open that recorded it and each later open it refuses.
func (e *QuarantineBlockError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{"migration.quarantine_version": e.Version}, nil
}

// UnsupportedSchemaVersionError is returned when Open finds a workspace whose
// schema is genuinely incompatible with this binary — the live tables do not
// match the binary's baseline shape. A workspace whose goose bookkeeping is
// merely ahead of the registry but whose application tables are intact does
// NOT yield this error; an ahead goose log is a tolerated no-op (goose treats
// unknown-ahead rows as nothing-to-apply), so only a missing baseline refuses.
//
// [LAW:one-source-of-truth] MaxSupported is migrations.MaxVersion() — the same
// value that bounds "pending". There is no second "max supported" constant to
// drift from the registry, so no startup assertion is needed to keep them
// coherent: they are the same number.
//
// [LAW:types-are-the-program] MissingBaseline carries the schema gaps that
// classify the workspace as genuinely incompatible (vs. recoverable). Its
// presence is the discriminator: empty means the application schema is fine
// (and the binary should not be returning this error), non-empty names the
// specific tables/columns the binary cannot operate against.
type UnsupportedSchemaVersionError struct {
	WorkspaceVersion int64
	MaxSupported     int64
	MissingBaseline  []string
	// MinWriterSchema is the workspace's minimum writer schema, 0 when none
	// was read. Above MaxSupported it is why this binary may read the
	// workspace but not write to it: a migration this binary lacks changed a
	// write rule.
	MinWriterSchema int64
	// MinWriterSchemaErr is why the workspace's minimum writer schema is not a
	// schema version. The minimum is then unknown, so this binary reads the
	// workspace but does not write to it.
	MinWriterSchemaErr error
	// SnapshotName is the newest migration-recovery snapshot holding a
	// workspace this binary writes (searchRollbackSnapshot), when one exists.
	// Populating it offers the user a lossy rollback path; emptying it means no
	// snapshot this binary can open was found.
	// [LAW:types-are-the-program] Which rollback line the message emits is
	// encoded by whether the optional field is populated, not by a flag.
	SnapshotName string
	// SnapshotLookupErr is why snapshots could not be read, when some could
	// not: the listing, or a snapshot newer than SnapshotName that may have
	// been a closer rollback. [LAW:no-silent-failure] a failed read is reported
	// as one, never as "no snapshot exists".
	SnapshotLookupErr error
	// SnapshotsExamined counts the migration snapshots the search read, and
	// SnapshotsUnreadable those it could not.
	SnapshotsExamined, SnapshotsUnreadable int
}

func (e *UnsupportedSchemaVersionError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"please upgrade lit (your workspace is at schema version %d; this binary supports up to %d",
		e.WorkspaceVersion, e.MaxSupported,
	)
	if len(e.MissingBaseline) > 0 {
		fmt.Fprintf(&b, "; the live schema is also missing baseline shape: %s",
			strings.Join(e.MissingBaseline, ", "))
	}
	if e.MinWriterSchema > e.MaxSupported {
		fmt.Fprintf(&b, "; schema version %d changed a write rule this binary predates, so it reads this workspace but does not write to it or sync it",
			e.MinWriterSchema)
	}
	if e.MinWriterSchemaErr != nil {
		fmt.Fprintf(&b, "; its minimum writer schema cannot be read (%v), so this binary reads this workspace but does not write to it or sync it",
			e.MinWriterSchemaErr)
	}
	b.WriteString(")")
	// The workspace is ahead of this binary, so the lossless fix is a NEWER
	// binary, never a schema reverse. The remedy names the schema version that
	// binary must support, not the build that wrote it: a recorded build can be
	// describe-stamped, which no release feed resolves. Bare `lit upgrade` is
	// always executable, and it refuses a release that cannot open this
	// workspace. RemoteSchemaAheadError names the same remedy for the remote
	// boundary. [LAW:one-type-per-behavior]
	// [LAW:dataflow-not-control-flow] The supported path renders on every
	// refusal; only the lossy rollback line varies, with SnapshotName.
	fmt.Fprintf(&b,
		"\n\nto operate this workspace, install a lit that supports schema version %d:\n  lit upgrade\n\n(this is the supported path — `lit upgrade` runs even from this too-old binary, installs the latest release, and refuses a release that cannot operate this workspace.)",
		e.WorkspaceVersion)
	switch {
	case e.SnapshotName != "":
		fmt.Fprintf(&b,
			"\n\nif you are stuck and need to roll back this workspace to match this binary:\n  lit snapshots restore %s\n\nthis is a LOSSY recovery — any data written under the newer binary will be discarded.",
			e.SnapshotName)
	case e.SnapshotLookupErr == nil:
		b.WriteString("\n\nno migration snapshot holds this workspace's data in a shape this binary writes; lossy rollback is not possible from this workspace.")
	}
	if e.SnapshotLookupErr != nil {
		fmt.Fprintf(&b, "\n\nsome snapshots could not be read while looking for one to roll this workspace back to: %v", e.SnapshotLookupErr)
	}
	return b.String()
}

// EventRecord is the schema the refusal met, which snapshot it offered as the
// lossy rollback — "" when none — why snapshots could not be read, and how
// many it read.
func (e *UnsupportedSchemaVersionError) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
			"schema_ahead.workspace_schema":        e.WorkspaceVersion,
			"schema_ahead.running_schema_max":      e.MaxSupported,
			"schema_ahead.min_writer_schema":       e.MinWriterSchema,
			"schema_ahead.min_writer_schema_error": telemetry.ErrorText(e.MinWriterSchemaErr),
			"schema_ahead.rollback_snapshot":       e.SnapshotName,
			"schema_ahead.rollback_lookup_error":   telemetry.ErrorText(e.SnapshotLookupErr),
		}, map[string]int{
			"schema_ahead.snapshots_examined":   e.SnapshotsExamined,
			"schema_ahead.snapshots_unreadable": e.SnapshotsUnreadable,
		}
}

// SchemaAheadOfBinary: only a lit whose schema support reaches the
// workspace's opens it.
func (*UnsupportedSchemaVersionError) SchemaAheadOfBinary() {}

var (
	_ model.RecordedError = (*UnsupportedSchemaVersionError)(nil)
	_ model.SchemaAhead   = (*UnsupportedSchemaVersionError)(nil)
)

// VersionContentMismatchError is returned by verifyAppliedVersionsMatchRegistry
// when it finds a goose-applied migration version whose CURRENT registry
// content did not land on this workspace's live schema. goose_db_version
// records only a version number and an applied bit — never the migration's
// content — so it cannot itself tell "version N ran and its effect is on
// disk" from "version N is marked applied, but the registry's current
// migration N is unrelated content that never actually ran here" (e.g. after
// a baseline rewrite reused version slots for different migrations). Without
// this check that second case reads as "fully migrated" and the missing
// effect never surfaces.
//
// runMigration treats this as a self-heal trigger, not a terminal refusal:
// on sight it calls repairVersionContentDrift and, on a clean repair, Open
// proceeds with no error ever reaching the caller. This type — and its
// named version/file/gap — only escapes to the caller when the REPAIR itself
// fails, wrapped with repair context, or from direct callers of
// verifyAppliedVersionsMatchRegistry that want the pure detection signal.
//
// [LAW:no-silent-failure] The workspace is not corrupt — every column it
// actually has is intact — so detection is loud and specific about the
// version and gap even though the common-path outcome is a silent repair,
// not a surfaced error.
type VersionContentMismatchError struct {
	Version int64
	Name    string
	// Missing names every table.column this version's registered content
	// should have produced but the live schema lacks.
	Missing []string
}

func (e *VersionContentMismatchError) Error() string {
	return fmt.Sprintf(
		"migration v%d %q is recorded as applied, but its registered content is missing from this workspace's live schema: %s\n\n"+
			"this usually means the version number was reused for different historical content after this workspace last migrated — "+
			"the recorded applied version does not reflect what actually ran here",
		e.Version, e.Name, strings.Join(e.Missing, ", "),
	)
}

// gooseVersionTable is goose's history table; its presence is the discriminator
// between a goose-managed workspace and a pre-goose / fresh one.
const gooseVersionTable = "goose_db_version"

// baselineVersion re-exports migrations.Baseline. The const lives in the
// migrations package because it is a property of the embedded registry; this
// alias keeps the existing store-internal call sites readable without
// re-typing the import path.
//
// [LAW:one-source-of-truth] One numeric definition (migrations.Baseline); this
// is a typed reference, not a duplicate value.
const baselineVersion = migrations.Baseline

// migrationPhase is the workspace's position relative to the goose registry,
// derived once from side-effect-free probes. The runner acts on the phase; it
// never re-derives state from scattered checks.
// [LAW:types-are-the-program] The phase is the discriminator that decides
// stamping vs applying; illegal mixed states (a partial pre-goose schema) are
// not a phase — they are a classification error that fails loudly.
type migrationPhase int

const (
	// phaseFresh: no goose table and no canonical tables. goose applies the
	// baseline (and any later migrations) from scratch.
	phaseFresh migrationPhase = iota
	// phaseAdopt: no goose table but the full canonical schema is present.
	// Stamp the baseline version, then apply any later migrations.
	phaseAdopt
	// phaseManaged: goose table present. Apply whatever versions exceed the
	// recorded one (possibly none).
	phaseManaged
)

// migrationState is the classified migration position plus the recorded version
// (meaningful only for phaseManaged).
type migrationState struct {
	phase          migrationPhase
	appliedVersion int64
	// support is this binary's schema support as the classified state saw it.
	support version.SchemaSupport
}

// willMutate reports whether this Open will write. Fresh and adopt always
// write (baseline apply / version stamp); a managed workspace writes only when
// it trails the registry. The snapshot guard fires exactly when this is true.
func (st migrationState) willMutate() bool {
	switch st.phase {
	case phaseManaged:
		return st.appliedVersion < st.support.Max
	default:
		return true
	}
}

// migrate is the single startup migration boundary. It owns the snapshot
// guard: runMigration takes exactly one recovery snapshot before its first
// write, and migrate prunes migration snapshots at the tail of a mutating
// Open. It reports what it did as a storage.Migrated, and a failure as one
// *SchemaConvergenceError naming the step that failed. A prune that ran after
// the work it trails committed is reported on the outcome, never as its error.
//
// [LAW:single-enforcer] Store-level Open routes all schema convergence through
// this one boundary; the snapshot/prune budget lives here, not at callsites.
func (s *Store) migrate(ctx context.Context) (storage.Migrated, error) {
	guard := newSnapshotGuard(s.doltRootDir, formatMigrationSnapshotLabel(time.Now()))
	op, pruneErr, fail := s.runMigration(ctx, guard)
	if fail != nil {
		fail.PruneErr = pruneErr
		return storage.Migrated{}, fail
	}
	done := storage.Migrated{SchemaAhead: op.schemaAhead(), PruneErr: pruneErr}
	snap, ok := guard.took()
	if !ok {
		return done, nil
	}
	done.Snapshot = recoverySnapshot(snap)
	// [LAW:single-enforcer] Automatic-snapshot retention is one rule
	// (pruneAutomaticSnapshots); user snapshots share the directory under an
	// independent budget the rule never touches.
	done.PruneErr = errors.Join(done.PruneErr, prune(pruneMigrationSnapshots, "prune migration snapshots", func() error {
		_, err := s.pruneAutomaticSnapshots(ctx)
		return err
	}))
	return done, nil
}

// Migrated reports what this handle's open did to the stored shape. A handle
// opened for sync reports its bootstrap and no migration; a read open, which
// never builds, reports no bootstrap and whatever its migration did.
func (s *Store) Migrated() storage.Migrated {
	return s.migrated
}

// migrationAssessment is what a workspace's live schema is relative to this
// binary's registry, derived from reads alone: the classified phase and
// version position, and the content drift (registry-declared columns the
// live schema is missing for versions the goose log already claims) that a
// write open repairs. It is the one fact both open paths act on — the write
// open goes on to mutate under the commit lock, the read open decides whether
// it may serve without one — so neither can classify the workspace by a rule
// the other lacks. [LAW:one-source-of-truth]
type migrationAssessment struct {
	state migrationState
	// op is what this binary may do with the workspace.
	op operability
	// drift is nil when every applied version's registered content is
	// present on the live schema.
	drift *VersionContentMismatchError
}

// needsWrite reports whether a migrate of this workspace would mutate it: a
// fresh or pre-goose workspace, a managed one trailing the registry, or one
// whose applied versions have drifted from their registered content. A read
// open that observes false may serve without the commit lock; one that
// observes true hands off to the write open. [LAW:one-source-of-truth] the
// schema position and content that decide whether migrate writes are the
// facts that decide whether the read open hands off, so a reader cannot
// serve a schema migrate would have changed. (The write open's other
// normalization, the default-branch rename, is not a schema fact and is not
// assessed here.)
func (a migrationAssessment) needsWrite() bool {
	return a.drift != nil || a.state.willMutate()
}

// String names what the assessment found, for the read open's handoff error.
func (a migrationAssessment) String() string {
	switch {
	case a.drift != nil:
		return fmt.Sprintf("applied schema v%d missing registered content (%s)", a.drift.Version, strings.Join(a.drift.Missing, ", "))
	case a.state.phase == phaseManaged:
		return fmt.Sprintf("schema v%d behind this binary's v%d", a.state.appliedVersion, a.state.support.Max)
	case a.state.phase == phaseAdopt:
		return "a pre-goose schema awaiting adoption"
	default:
		return "an empty schema awaiting its baseline"
	}
}

// assessMigration classifies the workspace and verifies it with reads only:
// no snapshot, no commit, no lock beyond the engine's own. It refuses a
// workspace this binary may not read, carries whether it may write for the
// write open to decide, and surfaces any error other than content drift,
// which it carries as data for the caller to act on.
//
// [LAW:no-silent-failure] The content verification runs for every
// phaseManaged workspace, independent of willMutate — a version-content
// mismatch on an already-applied version is invisible to goose bookkeeping
// whether or not later migrations are pending, so a workspace with nothing
// pending must still be checked (that is exactly the "reported as fully
// migrated" failure this guards against). [LAW:single-enforcer] assessContent is the
// only call site for verifyAppliedVersionsMatchRegistry. Only phaseManaged
// carries goose-recorded applied versions to check content for; phaseFresh
// has none yet and phaseAdopt's pre-goose shape is verified by
// verifyIssuesReconcilable/reconcileToBaseline instead.
func (s *Store) assessMigration(ctx context.Context) (migrationAssessment, *SchemaConvergenceError) {
	state, op, fail := s.classify(ctx)
	if fail != nil {
		return migrationAssessment{}, fail
	}
	return s.assessContent(ctx, state, op)
}

// classify is every open's first convergence step: the workspace's phase and
// what this binary may do with it, refusing one it may not read.
func (s *Store) classify(ctx context.Context) (migrationState, operability, *SchemaConvergenceError) {
	var state migrationState
	var op operability
	fail := attempt(stepClassify, func() (err error) {
		state, op, err = s.assessOperability(ctx)
		return err
	})
	return state, op, fail
}

// checkWritable classifies the workspace and applies the floor a write rule
// sets, for every open whose engine writes: the migrating open and the sync
// open. [LAW:single-enforcer] one function, so both opens report a failure of
// either step as the same step.
func (s *Store) checkWritable(ctx context.Context) (migrationState, operability, *SchemaConvergenceError) {
	state, op, fail := s.classify(ctx)
	if fail != nil {
		return migrationState{}, operability{}, fail
	}
	return state, op, attempt(stepRefuseUnwritable, func() error { return s.refuseUnwritable(ctx, op) })
}

// assessContent verifies a managed workspace's applied versions against the
// registry and carries the content drift it finds as data, for the write open
// to repair and the read open to hand off on.
func (s *Store) assessContent(ctx context.Context, state migrationState, op operability) (migrationAssessment, *SchemaConvergenceError) {
	if op.ahead() || state.phase != phaseManaged {
		return migrationAssessment{state: state, op: op}, nil
	}
	var mismatch *VersionContentMismatchError
	fail := attempt(stepVerifyContent, func() error {
		err := s.verifyAppliedVersionsMatchRegistry(ctx, state.appliedVersion)
		if errors.As(err, &mismatch) {
			return nil
		}
		return err
	})
	if fail != nil {
		return migrationAssessment{}, fail
	}
	return migrationAssessment{state: state, op: op, drift: mismatch}, nil
}

// runMigration classifies the workspace once, refuses one this binary may not
// write, snapshots before the first write, adopts a pre-goose workspace if
// needed, then applies pending migrations one Dolt commit each. It returns what
// this binary may do with the workspace, for the open to report, and why a
// prune failed, beside its failure or on its own.
//
// [LAW:single-enforcer] One runner owns migration ordering and the snapshot/
// commit boundary; goose is its only changeset registry and no other code
// applies schema.
// [LAW:dataflow-not-control-flow] The same assess -> repair -> snapshot ->
// adopt -> apply sequence runs every Open; variability lives in the phase and
// the set of pending versions, not in whether stages execute.
func (s *Store) runMigration(ctx context.Context, guard *snapshotGuard) (op operability, pruneErr error, fail *SchemaConvergenceError) {
	state, op, fail := s.checkWritable(ctx)
	if fail != nil {
		return operability{}, nil, fail
	}
	assessment, fail := s.assessContent(ctx, state, op)
	if fail != nil {
		return operability{}, nil, fail
	}
	pruneErr, fail = s.applyMigration(ctx, guard, assessment)
	return op, pruneErr, fail
}

// applyMigration is runMigration's work on a workspace this binary may write,
// up to and including the recovery snapshot; applySnapshotted does the rest.
func (s *Store) applyMigration(ctx context.Context, guard *snapshotGuard, assessment migrationAssessment) (pruneErr error, fail *SchemaConvergenceError) {
	state := assessment.state
	// A detected content mismatch is not a terminal refusal: it is the signal
	// to self-heal. Unlike ensureQuarantineTable's detect-then-recreate
	// precedent (which only ever touches lit's own bookkeeping table, bounded
	// to empty-or-refuse), this repair runs ALTER TABLE against user-owned
	// tables (e.g. issues) automatically and unprompted — the same risk class
	// applyPendingMigrations exists to protect against — so
	// repairVersionContentDriftWithRollback gives it that same
	// checkpoint-and-reset discipline rather than the bookkeeping-table
	// precedent. The workspace converges to the registry's exact shape purely
	// by running lit — no operator SQL, ever. [LAW:no-silent-failure] A failure
	// repairing (as opposed to detecting) still aborts Open loudly; only a
	// clean repair is swallowed into a routine commit.
	if assessment.drift != nil {
		if pruneErr, fail = s.repairVersionContentDriftWithRollback(ctx, state.appliedVersion, assessment.drift); fail != nil {
			return pruneErr, fail
		}
	}
	if !state.willMutate() {
		return pruneErr, nil
	}
	// Fast-fail before the snapshot guard so a permanently-quarantined workspace
	// does not accumulate recovery snapshots on every Open.
	if fail := s.quarantineGate(ctx, state); fail != nil {
		return pruneErr, fail
	}
	// [LAW:no-silent-failure] Verify reconcile prerequisites BEFORE
	// the snapshot guard fires, so a workspace whose shape reconcile
	// cannot recover from does not accumulate a recovery snapshot per
	// Open. The check is gated to phaseAdopt because that is the only
	// phase where reconcile actually runs — phaseFresh has no issues
	// table to verify, and phaseManaged has already passed v1
	// adoption so its issues table shape is governed by the goose
	// registry, not by reconcile preconditions. Running the probe in
	// non-adopt phases would either no-op pointlessly or produce a
	// misleadingly-wrapped error.
	if state.phase == phaseAdopt {
		if fail := attempt(stepCheckReconcilePrerequisites, func() error { return s.verifyIssuesReconcilable(ctx) }); fail != nil {
			return pruneErr, fail
		}
	}
	var snap dbsnapshot.Taken
	if fail := attempt(stepSnapshot, func() (err error) {
		snap, err = guard.ensure(ctx)
		return err
	}); fail != nil {
		return pruneErr, fail
	}
	migrationPruneErr, fail := s.applySnapshotted(ctx, snapshotted{snap: snap}, guard, state)
	return errors.Join(pruneErr, migrationPruneErr), fail
}

// applySnapshotted is the convergence's work after the open took its recovery
// snapshot: adopt a pre-goose workspace if needed, then apply the pending
// migrations. Every failure it returns is built from p, so it carries the
// snapshot.
func (s *Store) applySnapshotted(ctx context.Context, p snapshotted, guard *snapshotGuard, state migrationState) (pruneErr error, fail *SchemaConvergenceError) {
	if state.phase == phaseAdopt {
		// [LAW:single-enforcer] Pre-goose workspaces at any historical
		// canonical shape forward-migrate through reconcileToBaseline
		// BEFORE the adoption stamp lands. The reconcile is idempotent:
		// a workspace already at v1 sees every probe return "present"
		// and reconcile no-ops; a workspace at an earlier shape (e.g.
		// missing issue_events or agent_prompt) gets its gaps filled.
		//
		// The reconcile is a HISTORICAL ARTIFACT — no new operations
		// get added here. Goose owns v1 → vN going forward.
		if fail := p.attempt(stepReconcile, func() error {
			_, err := s.reconcileToBaseline(ctx, guard)
			return err
		}); fail != nil {
			return nil, fail
		}
		// [LAW:no-silent-failure] After reconcile, verify the
		// workspace shape actually matches the baseline before
		// stamping. The reconcile's CREATE TABLE steps are gated on
		// table presence, NOT column presence — so a workspace that
		// has a malformed non-issues table (e.g. relations exists but
		// is missing the type column) would have reconcile skip the
		// CREATE and the malformed table would persist. Stamping v1
		// on that workspace would be a lie.
		// verifyBaselineShape compares against the baseline file and
		// names every remaining gap; if any gaps survive, refuse with
		// a structural error before the stamp lands.
		var missing []string
		if fail := p.attempt(stepVerifyReconciledShape, func() (err error) {
			_, missing, err = s.verifyBaselineShape(ctx)
			return err
		}); fail != nil {
			return nil, fail
		}
		if len(missing) > 0 {
			return nil, p.failAt(stepReconciledShapeDiffers, fmt.Errorf(
				"post-reconcile workspace shape still differs from baseline "+
					"(remaining gaps: %s); reconcile cannot bring this workspace "+
					"to v1 — the shape is structurally beyond what pre-goose "+
					"reconcile can recover",
				strings.Join(missing, ", "),
			))
		}
		if fail := s.adoptPreGooseWorkspace(ctx, p); fail != nil {
			return nil, fail
		}
		// commitWorkingSet (not ...Once) so the adoption stamp gets the
		// transient-manifest retry wrapper. migrate() already holds the commit
		// lock, so the nested withCommitLock short-circuits acquisition.
		if fail := p.attempt(stepCommitAdoptionStamp, func() error {
			return s.commitWorkingSet(ctx, fmt.Sprintf("migrate: adopt pre-goose workspace at v%d", baselineVersion))
		}); fail != nil {
			return nil, fail
		}
	}
	return s.applyPendingMigrations(ctx, p)
}

// upByOne applies the single next pending migration via goose. It is the ONE
// goose forward-step both drivers share: the durable startup path
// (applyPendingMigrations, which wraps each step in a Dolt commit +
// checkpoint/quarantine recovery) and the transient schema-lift
// (liftWorkingSetToRegistry, which commits nothing and only lifts a throwaway
// branch so it can be read). It is PURE — the startup path layers its
// test-failure injection ON TOP of this (see applyPendingMigrations), so the
// lift, which steps through here bare, is never affected by that hook.
// [LAW:single-enforcer] one goose stepper; [LAW:one-source-of-truth] both
// consume the same embedded registry via newGooseProvider, so neither mints a
// second schema-adaptation path.
func (s *Store) upByOne(ctx context.Context, provider *goose.Provider) (*goose.MigrationResult, error) {
	return stepResult(provider.UpByOne(ctx))
}

// stepResult is a goose step's outcome with the step's result beside its error
// even when the step failed: goose returns a nil result on failure and names
// the migration that failed only in the *goose.PartialError, so the result is
// taken from there. Without it a failing migration reaches earnsQuarantine as
// version 0 and is never quarantined. A failure goose hits before any step
// starts (preparing it, listing migrations) carries no PartialError and stays
// unidentified.
func stepResult(result *goose.MigrationResult, err error) (*goose.MigrationResult, error) {
	var partial *goose.PartialError
	if errors.As(err, &partial) {
		return partial.Failed, err
	}
	return result, err
}

// liftWorkingSetToRegistry advances the live working set to the registry's max
// schema version by applying every pending migration's DDL through goose — the
// same registry, the same stepper (upByOne) the durable startup path uses. It
// commits nothing to Dolt, takes no checkpoint, and takes no snapshot: its sole
// caller is the reconcile, which runs it on a THROWAWAY scratch branch that is
// hard-reset away or discarded immediately after the working set is read. It
// exists because reconcile must read/write a remote or base commit written by
// an older binary (a schema behind this binary's registry); before the lift the
// working set at that commit lacks columns this binary's SQL names, so reads
// (Export) and writes (the replay's delta) fail with a raw backend "no such
// column" error. Lifting reuses the migration chain rather than reimplementing
// schema adaptation in the merge engine. [LAW:one-source-of-truth] migrations
// are the single owned representation of schema change. [LAW:no-silent-failure]
// a migration failure here aborts the reconcile loudly; there is nothing to
// quarantine because nothing durable was written.
// [LAW:dataflow-not-control-flow] the loop runs every call; the recorded
// version at the reset commit decides how many migrations goose has to apply
// (zero when the commit is already at registry max — the common case, since the
// local head is always current).
func (s *Store) liftWorkingSetToRegistry(ctx context.Context) error {
	provider, err := newGooseProvider(s.db)
	if err != nil {
		return fmt.Errorf("construct schema-lift provider: %w", err)
	}
	for {
		result, err := s.upByOne(ctx, provider)
		if errors.Is(err, goose.ErrNoNextVersion) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lift working set to registry schema: %w", err)
		}
		// goose's contract ends the sequence with ErrNoNextVersion, never a
		// (nil, nil) result. If it ever did, this loop — which makes no
		// per-migration Dolt commit to terminate on — would spin forever with no
		// progress and no error. Fail loud instead of hanging silently.
		// [LAW:no-silent-failure] (The applied migration's identity is otherwise
		// discarded: this lift is a read-only schema bump on a throwaway branch.)
		if result == nil {
			return fmt.Errorf("lift working set to registry schema: goose returned no result and no error")
		}
	}
}

// applyPendingMigrations runs each pending migration through goose and records
// one Dolt commit per applied migration. Before the first migration runs it
// creates a Dolt checkpoint so a failure can reset the working set. On
// failure it quarantines the failed version (persisted after the reset) and
// returns the failed step, whose cause is the CheckpointResetError when the
// reset held. A checkpoint prune failure is returned beside it, or on its own
// once every migration committed.
//
// [LAW:single-enforcer] The checkpoint and per-migration commit boundary live
// here; goose is stepped only through upByOne (shared with the transient
// reconcile lift), and no other code checkpoints/commits/quarantines for
// migration purposes. The quarantine check lives in quarantineGate (called
// before the snapshot guard in runMigration) so the gate fires without a
// snapshot.
// [LAW:dataflow-not-control-flow] The same sequence (checkpoint → goose loop
// → prune) runs on every call; variability lives in the applied-vs-registry
// set, not in whether stages execute.
func (s *Store) applyPendingMigrations(ctx context.Context, p snapshotted) (pruneErr error, fail *SchemaConvergenceError) {
	// Construct the provider before creating the checkpoint so a provider
	// construction failure leaves no orphaned branch behind.
	var provider *goose.Provider
	if fail := p.attempt(stepMigrationProvider, func() (err error) {
		provider, err = newGooseProvider(s.db)
		return err
	}); fail != nil {
		return nil, fail
	}

	// Create Dolt checkpoint before any mutation. The quarantine table is
	// already committed (ensureQuarantineTable ran before this call), so
	// the checkpoint state includes it — the table survives a hard reset.
	var checkpoint storage.Checkpoint
	if fail := p.attempt(stepMigrationCheckpoint, func() (err error) {
		checkpoint, err = s.CreateCheckpoint(ctx, migrationCheckpointPrefix)
		return err
	}); fail != nil {
		return nil, fail
	}

	// The startup path is the only one that injects synthetic goose failures
	// (for the checkpoint/quarantine recovery tests); the hook stands in for
	// goose's step here, so the transient reconcile lift never sees it, and
	// its outcome passes through stepResult like goose's own.
	// [LAW:composability] [LAW:single-enforcer]
	step := func() (*goose.MigrationResult, error) {
		if hook := migrationUpByOneForTest; hook != nil {
			return stepResult(hook(ctx, s.db, provider))
		}
		return s.upByOne(ctx, provider)
	}
	pruneCheckpoints := func(ctx context.Context) error {
		return prune(pruneMigrationCheckpoints, "prune migration checkpoints", func() error {
			return s.PruneCheckpoints(ctx, migrationCheckpointPrefix, migrationCheckpointRetention)
		})
	}

	// recoverFrom resets the working set the run left part-way, records the
	// quarantine the failure earned, if any, and prunes. It runs to the end on
	// a context the caller cannot cancel: a caller that gave up (lit init's
	// adopt timeout cancels the context Open ran under) must not leave the
	// workspace half-migrated. [LAW:no-ambient-temporal-coupling]
	//
	// Prune on the failure path too: a quarantined failure blocks future
	// Opens before they create a new checkpoint, but a failure that earned no
	// quarantine leaves the door open for repeated retries to accumulate
	// branches. The prune failure rides beside the step's failure, never in
	// its cause, so it never decides how the failure is classified, and it
	// is still printed: it is the only sign those branches are piling up.
	// [LAW:no-silent-failure]
	recoverFrom := func(failed, reset ConvergenceStep, result *goose.MigrationResult, cause error, quarantine bool) (error, *SchemaConvergenceError) {
		recoveryCtx := context.WithoutCancel(ctx)
		fail := s.handleMigrationFailure(recoveryCtx, p, failed, reset, result, cause, quarantine, checkpoint)
		return pruneCheckpoints(recoveryCtx), fail
	}

	for {
		result, gooseErr := step()
		if errors.Is(gooseErr, goose.ErrNoNextVersion) {
			// Success: prune old checkpoints to the retention count. Every
			// migration has committed, so the run's outcome is settled: a
			// caller that gives up now cannot turn a migrated workspace into a
			// failed Open, and neither can a prune that fails, which is
			// reported on the open's outcome. [LAW:no-ambient-temporal-coupling]
			return pruneCheckpoints(context.WithoutCancel(ctx)), nil
		}
		if gooseErr != nil {
			return recoverFrom(stepMigration, stepMigrationReset, result, gooseErr, earnsQuarantine(ctx, result, gooseErr))
		}
		// commitWorkingSet (not ...Once) so each migration commit gets the
		// transient-manifest retry — startup migration is a critical path and a
		// recoverable Dolt manifest blip must not brick Open. The commit lock is
		// already held, so re-entering withCommitLock short-circuits. A commit
		// that still fails leaves the step applied but unversioned, so it is
		// reset like any other stop; it says nothing against the migration, so
		// it earns no quarantine. goose recorded the step before the commit, so
		// a reset that then fails leaves a complete step, not a partial one.
		if err := atFailpoint(stepCommitMigration, func() error {
			return s.commitWorkingSet(ctx, migrationCommitMessage(result))
		}); err != nil {
			return recoverFrom(stepCommitMigration, stepCommitMigrationReset, result, fmt.Errorf("commit migration v%d: %w", result.Source.Version, err), false)
		}
	}
}

// earnsQuarantine says whether a failed migration step earns a quarantine: it
// does for a version goose identified, whose step failed while its caller still
// wanted it run. A step cut short by the caller's cancellation, or by online-GC
// contention (goose wraps every error inside a step, the database's as well as
// the migration's), says nothing against the migration, so it earns none and
// the next open runs it again.
func earnsQuarantine(ctx context.Context, result *goose.MigrationResult, cause error) bool {
	version, _ := migrationIdentity(result)
	return version != 0 && ctx.Err() == nil && !isTransientGCContentionError(cause)
}

// migrationIdentity is the version and file name goose reported for a step, or
// zero values when it reported none.
func migrationIdentity(result *goose.MigrationResult) (int64, string) {
	if result == nil || result.Source == nil {
		return 0, ""
	}
	return result.Source.Version, filepath.Base(result.Source.Path)
}

// handleMigrationFailure resets the working set to the pre-migrate checkpoint,
// records quarantine, when the failure earned one, in the post-reset database,
// and returns the step that failed, its cause the outcome of the reset: failed,
// with a CheckpointResetError, when the recovery held; the quarantine step,
// with a CheckpointResetError whose QuarantineErr says why recording failed and
// whose Quarantine says whether one is in force, when recording did not
// complete; reset, with a CheckpointResetFailedError, when the reset failed.
//
// Ordering: reset first, quarantine second — the reset discards all working-set
// changes since the checkpoint, but the quarantine table itself was committed
// before the checkpoint, so it survives and the post-reset INSERT lands cleanly.
func (s *Store) handleMigrationFailure(ctx context.Context, p snapshotted, failed, reset ConvergenceStep, result *goose.MigrationResult, cause error, quarantine bool, checkpoint storage.Checkpoint) *SchemaConvergenceError {
	version, name := migrationIdentity(result)
	migration := &MigrationStepError{Version: version, Name: name, Cause: cause}

	if err := atFailpoint(reset, func() error { return s.ResetToCheckpoint(ctx, checkpoint.Name) }); err != nil {
		return p.failAt(reset, &CheckpointResetFailedError{Checkpoint: checkpoint, Cause: migration, ResetErr: err})
	}
	outcome := &CheckpointResetError{Checkpoint: checkpoint, Cause: migration}
	if !quarantine {
		return p.failAt(failed, outcome)
	}
	quarantined := migration.quarantine()
	if err := atFailpoint(stepRecordQuarantine, func() error {
		return s.recordQuarantine(ctx, quarantined)
	}); err != nil {
		outcome.QuarantineErr = err
		return p.failAt(stepRecordQuarantine, outcome)
	}
	// The row is in the working set from here on, so the quarantine is in
	// force whether or not its commit lands: an uncommitted row is what the
	// next open reads.
	outcome.Quarantine = quarantined
	if err := atFailpoint(stepCommitQuarantine, func() error {
		return s.commitWorkingSet(ctx, fmt.Sprintf("migrate: quarantine v%d %s", quarantined.Version, quarantined.Name))
	}); err != nil {
		outcome.QuarantineErr = fmt.Errorf("commit quarantine v%d: %w", quarantined.Version, err)
		return p.failAt(stepCommitQuarantine, outcome)
	}
	return p.failAt(failed, outcome)
}

// quarantineTableStmt is the canonical migration_quarantine shape. Shared by
// the fresh-create and self-heal-recreate paths in ensureQuarantineTable so
// they cannot diverge.
const quarantineTableStmt = `CREATE TABLE migration_quarantine (
	version    BIGINT NOT NULL,
	name       TEXT NOT NULL,
	error_text TEXT NOT NULL,
	created_at VARCHAR(64) NOT NULL,
	PRIMARY KEY (version)
)`

// canonicalQuarantineColumns is the migration_quarantine column set this
// binary understands. A table whose columns don't match exactly (e.g. an
// older lit build's version_id/reason/quarantined_at layout) is a stale shape
// ensureQuarantineTable self-heals.
var canonicalQuarantineColumns = []string{"version", "name", "error_text", "created_at"}

// ensureQuarantineTable creates migration_quarantine if it does not already
// exist, and self-heals it in place if a prior lit build left it in a shape
// this binary no longer recognizes. The table is created outside the goose
// migration batch so a goose rollback cannot erase it.
//
// [LAW:one-source-of-truth] migration_quarantine is the sole authority on
// "failed and not to be retried"; it is owned by workspace bootstrap, not
// by the goose migration log. A stale-shaped copy of it is a second,
// unreadable version of that authority, so Open corrects it in place rather
// than trusting CREATE TABLE IF NOT EXISTS to have produced the real one.
func (s *Store) ensureQuarantineTable(ctx context.Context) error {
	cols, err := s.tableColumns(ctx, "migration_quarantine")
	if err != nil {
		return fmt.Errorf("ensure migration_quarantine table: %w", err)
	}
	if len(cols) == 0 {
		if _, err := s.db.ExecContext(ctx, quarantineTableStmt); err != nil {
			return fmt.Errorf("ensure migration_quarantine table: %w", err)
		}
		return nil
	}
	if quarantineShapeMatches(cols) {
		return nil
	}
	return s.recreateQuarantineTable(ctx, cols)
}

// quarantineShapeMatches reports whether cols is exactly the canonical
// migration_quarantine column set — not a subset or superset, since either
// means a shape this binary did not produce and should not assume it
// understands.
func quarantineShapeMatches(cols map[string]bool) bool {
	if len(cols) != len(canonicalQuarantineColumns) {
		return false
	}
	for _, c := range canonicalQuarantineColumns {
		if !cols[c] {
			return false
		}
	}
	return true
}

// recreateQuarantineTable drops and rebuilds a migration_quarantine table
// left in a non-canonical shape by an older lit build. The table is
// bootstrap/meta state that lit alone writes to, so replacing an empty one is
// safe. A non-empty mismatched table might hold genuine quarantine history
// under unrecognized column names, which is beyond what an automatic
// self-heal should guess at.
//
// [LAW:no-silent-failure] A row-bearing shape mismatch refuses loudly with
// the columns and row count named, rather than silently discarding data.
func (s *Store) recreateQuarantineTable(ctx context.Context, cols map[string]bool) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_quarantine`).Scan(&count); err != nil {
		return fmt.Errorf("ensure migration_quarantine table: count rows in stale-shape table: %w", err)
	}
	if count > 0 {
		names := make([]string, 0, len(cols))
		for c := range cols {
			names = append(names, c)
		}
		sort.Strings(names)
		// The one exit of this step that every open repeats, so it names its
		// own step. [LAW:types-are-the-program]
		return failAt(stepQuarantineTableShape, fmt.Errorf(
			"migration_quarantine has a non-canonical shape (columns: %s) and %d row(s) of history; "+
				"refusing to recreate automatically — this needs manual triage, not self-heal",
			strings.Join(names, ", "), count,
		))
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE migration_quarantine`); err != nil {
		return fmt.Errorf("ensure migration_quarantine table: drop stale-shape table: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, quarantineTableStmt); err != nil {
		return fmt.Errorf("ensure migration_quarantine table: recreate: %w", err)
	}
	return nil
}

// quarantineGate refuses a write open with the quarantine in force: the
// lowest quarantined version above the applied one.
//
// It first ensures the quarantine table exists — and self-heals it to the
// canonical shape if a prior lit build left it in an older one — because
// reading a stale-shaped table fails with a raw column-not-found SQL error.
// The table is committed at once, so it survives a checkpoint reset should one
// fire later in the open.
//
// For phaseAdopt, adoptPreGooseWorkspace will stamp baselineVersion before any
// migration runs, so the effective applied version is baselineVersion — a
// quarantine row for the baseline itself must not block after adoption confirms
// the schema is present.
//
// [LAW:single-enforcer] The one gate: migrate refuses on it, and Unquarantine
// clears what it refuses on. [LAW:no-ambient-temporal-coupling] the read
// cannot precede the ensure, because no caller can reach one without the
// other. Quarantine table creation is decoupled from goose migrations so a
// goose rollback cannot erase the table it depends on.
func (s *Store) quarantineGate(ctx context.Context, state migrationState) *SchemaConvergenceError {
	if fail := attempt(stepEnsureQuarantineTable, func() error { return s.ensureQuarantineTable(ctx) }); fail != nil {
		return fail
	}
	if fail := attempt(stepCommitQuarantineTable, func() error {
		return s.commitWorkingSet(ctx, "migrate: ensure migration_quarantine table")
	}); fail != nil {
		return fail
	}
	effectiveApplied := state.appliedVersion
	if state.phase == phaseAdopt {
		effectiveApplied = baselineVersion
	}
	return attempt(stepCheckQuarantine, func() error { return s.checkPendingQuarantine(ctx, effectiveApplied) })
}

// checkPendingQuarantine returns a QuarantineBlockError for the lowest
// quarantined version above appliedVersion, its Next the one after it, and
// nil when none is quarantined.
func (s *Store) checkPendingQuarantine(ctx context.Context, appliedVersion int64) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT version, name, error_text FROM migration_quarantine WHERE version > ? ORDER BY version LIMIT 2`,
		appliedVersion,
	)
	if err != nil {
		return fmt.Errorf("check pending quarantine: %w", err)
	}
	defer rows.Close()
	var inForce []QuarantineBlockError
	for rows.Next() {
		var q QuarantineBlockError
		if err := rows.Scan(&q.Version, &q.Name, &q.ErrorText); err != nil {
			return fmt.Errorf("check pending quarantine: %w", err)
		}
		inForce = append(inForce, q)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("check pending quarantine: %w", err)
	}
	if len(inForce) == 0 {
		return nil
	}
	blocked := inForce[0]
	if len(inForce) > 1 {
		blocked.Next = &QuarantinedMigration{Version: inForce[1].Version, Name: inForce[1].Name}
	}
	return &blocked
}

// recordQuarantine upserts the quarantine row for q's migration version.
// ON DUPLICATE KEY UPDATE handles the case where a previous run already
// recorded a quarantine row for this version (e.g., operator cleared and
// retried, failed again).
func (s *Store) recordQuarantine(ctx context.Context, q *QuarantineBlockError) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO migration_quarantine (version, name, error_text, created_at) VALUES (?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE name = VALUES(name), error_text = VALUES(error_text), created_at = VALUES(created_at)`,
		q.Version, q.Name, q.ErrorText, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("record quarantine for v%d: %w", q.Version, err)
	}
	return nil
}

// classifyMigrationState derives the workspace phase using only reads,
// so a no-op Open performs no writes (it must take no snapshot).
//
// Three phases:
//   - phaseManaged: goose_db_version table present; goose owns the workspace.
//   - phaseFresh:   no goose table AND no canonical tables; brand new.
//   - phaseAdopt:   no goose table BUT at least one canonical table present.
//     The workspace is pre-goose at SOME historical canonical
//     shape (current or earlier). reconcileToBaseline (an
//     idempotent, probe-driven forward migrator)
//     brings any earlier shape forward to v1 before adoption
//     stamps. There is no "partial-and-illegal" refusal —
//     any presence of canonical tables means "pre-goose
//     workspace, reconcile-then-adopt."
//
// [LAW:types-are-the-program] Three phases, each with a forward path. No
// refusal branch. A "partial schema, restore or recreate" failure mode —
// which would destroy real user data with old canonical shapes — does not
// exist by construction here.
//
// [LAW:dataflow-not-control-flow] The classify function reads facts about
// the workspace; the runner reacts to them. No flags, no modes, no
// "what kind of corruption is this" guessing.
func (s *Store) classifyMigrationState(ctx context.Context) (migrationState, error) {
	support, err := version.CarriedSchemaSupport()
	if err != nil {
		return migrationState{}, err
	}
	// [LAW:types-are-the-program] Disk shape outranks goose bookkeeping
	// when the two disagree. issue_history is a pre-goose-only table —
	// the canonical baseline does not create it; reconcileToBaseline
	// drops it as part of the legacy→v1 bridge. Its presence is
	// conclusive evidence the workspace last reached a pre-goose
	// canonical shape, regardless of what goose_db_version claims.
	//
	// This probe re-routes "buggy older binary fabricated goose rows on
	// a pre-goose workspace" — which would otherwise trap in phaseManaged —
	// back into phaseAdopt where the bridge can run. The fabricated rows
	// are wiped by reconcile and adoption restamps cleanly.
	//
	// [LAW:one-source-of-truth] The live schema is canonical; the
	// goose log is derived state that should reflect it. When they
	// disagree, the schema wins and the log is reconciled to match.
	legacyMarker, err := s.tableExists(ctx, "issue_history")
	if err != nil {
		return migrationState{}, err
	}
	if legacyMarker {
		return migrationState{phase: phaseAdopt, support: support}, nil
	}
	gooseManaged, err := s.tableExists(ctx, gooseVersionTable)
	if err != nil {
		return migrationState{}, err
	}
	if gooseManaged {
		applied, err := s.recordedMigrationVersion(ctx)
		if err != nil {
			return migrationState{}, err
		}
		return migrationState{phase: phaseManaged, appliedVersion: applied, support: support}, nil
	}
	present, _, err := s.verifyBaselineShape(ctx)
	if err != nil {
		return migrationState{}, err
	}
	if present == 0 {
		return migrationState{phase: phaseFresh, support: support}, nil
	}
	return migrationState{phase: phaseAdopt, support: support}, nil
}

// migrationColumnAdds is one registry migration's version/name plus the
// table.column targets its Up section's ADD COLUMN statements register.
type migrationColumnAdds struct {
	version int64
	name    string
	adds    []tableColumnTarget
}

// registryColumnAddsThroughVersion returns, in ascending version order, the
// ADD COLUMN targets every registry migration strictly above the baseline and
// at or below maxVersion registers. It reads CURRENT registry content — the
// content a version number carries in this binary's embedded registry now,
// not necessarily what an older binary actually ran under that same version
// number. That gap between "recorded applied" and "current registered
// content" is exactly what verifyAppliedVersionsMatchRegistry checks for.
func registryColumnAddsThroughVersion(maxVersion int64) ([]migrationColumnAdds, error) {
	registry, err := migrations.Embedded()
	if err != nil {
		return nil, err
	}
	var out []migrationColumnAdds
	for _, m := range registry.Migrations() {
		if m.Version <= baselineVersion || m.Version > maxVersion {
			continue
		}
		data, err := migrations.FS.ReadFile(m.Name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", m.Name, err)
		}
		adds, err := parseAddColumnTargets(m.Name, gooseUpSection(string(data)))
		if err != nil {
			return nil, err
		}
		out = append(out, migrationColumnAdds{version: m.Version, name: m.Name, adds: adds})
	}
	return out, nil
}

// missingContentTarget names one ADD COLUMN target the registry defines for
// a specific version that the live schema is missing.
type missingContentTarget struct {
	version int64
	name    string
	table   string
	column  string
	stmt    string
}

// missingVersionContent probes the live schema against every ADD COLUMN
// target the registry defines for versions 1..appliedVersion, in ascending
// version order, and returns every target whose column is absent. It is the
// single probe verifyAppliedVersionsMatchRegistry (which reports only the
// earliest version's gaps) and repairVersionContentDrift (which repairs
// every gap) both build on — one place to define what "missing" means, so
// the two cannot silently disagree about it.
//
// [LAW:one-source-of-truth] The registry (migrations.FS), read via the same
// gooseUpSection parsing baselineSchema uses, is the sole definition of what
// each version should have produced; there is no second hand-maintained list
// of per-version effects to drift from it.
func (s *Store) missingVersionContent(ctx context.Context, appliedVersion int64) ([]missingContentTarget, error) {
	adds, err := registryColumnAddsThroughVersion(appliedVersion)
	if err != nil {
		return nil, err
	}
	tableCols := map[string]map[string]bool{}
	var missing []missingContentTarget
	for _, m := range adds {
		for _, target := range m.adds {
			cols, ok := tableCols[target.table]
			if !ok {
				cols, err = s.tableColumns(ctx, target.table)
				if err != nil {
					return nil, err
				}
				tableCols[target.table] = cols
			}
			if cols[target.column] {
				continue
			}
			missing = append(missing, missingContentTarget{
				version: m.version, name: m.name, table: target.table, column: target.column, stmt: target.stmt,
			})
		}
	}
	return missing, nil
}

// verifyAppliedVersionsMatchRegistry checks that every migration version this
// workspace's goose log claims as applied (1..appliedVersion) actually
// produced its registered ADD COLUMN content on the live schema. It reports
// the earliest mismatched version, mirroring checkPendingQuarantine's
// earliest-first ordering. missingVersionContent returns gaps in ascending
// version order with each version's targets contiguous, so the first run of
// matching versions in the slice is exactly that earliest version's gaps.
func (s *Store) verifyAppliedVersionsMatchRegistry(ctx context.Context, appliedVersion int64) error {
	missing, err := s.missingVersionContent(ctx, appliedVersion)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	earliest := missing[0]
	var names []string
	for _, m := range missing {
		if m.version != earliest.version {
			break
		}
		names = append(names, m.table+"."+m.column)
	}
	return &VersionContentMismatchError{Version: earliest.version, Name: earliest.name, Missing: names}
}

// repairVersionContentDrift applies every ADD COLUMN statement the current
// registry defines for versions 1..appliedVersion whose target column is
// still missing from the live schema, and returns a "table.column (vN name)"
// description of each one repaired, for the Dolt commit message. It repairs
// every gap missingVersionContent reports — not just the earliest one
// verifyAppliedVersionsMatchRegistry surfaces — so a workspace with drift at
// multiple version slots (the go-template-js shape: v2 and v3 both missing
// their column) converges to the registry's exact shape in one Open, not one
// repair per invocation.
//
// [LAW:one-source-of-truth] Re-executes the registry's own statement text
// verbatim (captured by parseAddColumnTargets), so a repaired column is
// byte-identical to what a fresh apply of that migration would have
// produced — there is no second, hand-maintained repair-DDL to drift from
// the registry.
// [LAW:no-silent-failure] An ALTER failure here — anything beyond "column
// already exists", which missingVersionContent's probe already excludes —
// aborts loudly with the statement and cause named; it is never swallowed.
//
// Scope matches detection exactly, not migration parity: parseAddColumnTargets
// (and therefore missingVersionContent) only ever registers ADD COLUMN
// targets, so a version's ADD CONSTRAINT / data-backfill statements (e.g.
// 00003_add_resolution.sql's issues_resolution_check, or 00004's
// redirect_target backfill) are never re-applied here even when the column
// they depend on was just repaired. This mirrors the same documented,
// intentional gap on verifyAppliedVersionsMatchRegistry/registryColumnAddsThroughVersion
// (CREATE TABLE, ADD CONSTRAINT-only, DROP/RENAME COLUMN, and data-only
// migrations are not tracked as content to verify or repair yet) — widening
// it is a job for whatever ticket widens detection, not this repair.
func (s *Store) repairVersionContentDrift(ctx context.Context, appliedVersion int64) ([]string, error) {
	missing, err := s.missingVersionContent(ctx, appliedVersion)
	if err != nil {
		return nil, err
	}
	var repaired []string
	for _, target := range missing {
		if _, err := s.db.ExecContext(ctx, target.stmt); err != nil {
			return nil, fmt.Errorf("repair v%d %q: apply %q: %w", target.version, target.name, target.stmt, err)
		}
		repaired = append(repaired, fmt.Sprintf("%s.%s (v%d %s)", target.table, target.column, target.version, target.name))
	}
	return repaired, nil
}

// migrationDriftRepairCommitMessage is the Dolt commit message for a
// version-content drift repair. It names every table.column repaired so the
// Dolt log carries the audit trail even though the repair happens
// transparently and no error reaches the operator.
func migrationDriftRepairCommitMessage(repaired []string) string {
	return fmt.Sprintf("migrate: repair version-content drift (%s)", strings.Join(repaired, ", "))
}

// repairVersionContentDriftWithRollback wraps repairVersionContentDrift with
// the same checkpoint-then-reset-on-failure discipline applyPendingMigrations
// gives ordinary migrations: a repair that applies one target's ALTER and
// then fails on the next must not leave the working set half-repaired with no
// way back, and a repair whose commit fails must not leave its ALTERs applied
// for the next unrelated commit to version. mismatch names the version/file that triggered the repair, for
// the error message only — the repair itself sweeps every gap through
// appliedVersion, not just mismatch's earliest one.
//
// [LAW:no-silent-failure] A reset failure is itself surfaced, never silently
// dropped, mirroring handleMigrationFailure's ordering below. The checkpoint
// prune runs after the repair committed, so its failure is returned beside a
// nil failure, for the open to report, not to fail on.
func (s *Store) repairVersionContentDriftWithRollback(ctx context.Context, appliedVersion int64, mismatch *VersionContentMismatchError) (pruneErr error, fail *SchemaConvergenceError) {
	var checkpoint storage.Checkpoint
	if fail := attempt(stepDriftCheckpoint, func() (err error) {
		checkpoint, err = s.CreateCheckpoint(ctx, migrationDriftRepairCheckpointPrefix)
		return err
	}); fail != nil {
		return nil, fail
	}
	repair := func() ([]string, error) {
		if hook := versionContentDriftRepairForTest; hook != nil {
			return hook(ctx)
		}
		return s.repairVersionContentDrift(ctx, appliedVersion)
	}
	repaired, repairErr := repair()
	if repairErr == nil {
		if commitErr := s.commitWorkingSet(ctx, migrationDriftRepairCommitMessage(repaired)); commitErr != nil {
			repairErr = fmt.Errorf("commit version-content drift repair: %w", commitErr)
		}
	}
	if repairErr != nil {
		repairErr = fmt.Errorf("repair version-content drift (detected at v%d %q): %w", mismatch.Version, mismatch.Name, repairErr)
		// The reset runs to the end on a context the caller cannot cancel, as
		// applyPendingMigrations' recovery does. [LAW:no-ambient-temporal-coupling]
		if resetErr := atFailpoint(stepDriftRepairReset, func() error {
			return s.ResetToCheckpoint(context.WithoutCancel(ctx), checkpoint.Name)
		}); resetErr != nil {
			return nil, failAt(stepDriftRepairReset, &CheckpointResetFailedError{Checkpoint: checkpoint, Cause: repairErr, ResetErr: resetErr})
		}
		return nil, failAt(stepDriftRepair, &CheckpointResetError{Checkpoint: checkpoint, Cause: repairErr})
	}
	return prune(pruneDriftCheckpoints, "prune version-content drift repair checkpoints", func() error {
		return s.PruneCheckpoints(context.WithoutCancel(ctx), migrationDriftRepairCheckpointPrefix, migrationCheckpointRetention)
	}), nil
}

// AppliedSchemaVersion reports the schema version currently recorded for this
// workspace — the goose-managed applied version, or 0 for a fresh/pre-goose
// (adopt) workspace that has not been stamped yet. It is the same value
// DowngradeTargetAheadError compares a downgrade target against, exported so the
// `lit upgrade` composer can make the symmetric refusal ("target binary's schema
// is behind this workspace — use lit downgrade") without duplicating the
// classification.
//
// [LAW:one-source-of-truth] classifyMigrationState is the sole authority on
// applied version; this is a typed read over it, not a second derivation.
// [LAW:effects-at-boundaries] a pure read — no snapshot, no mutation — so the
// upgrade command can consult it before deciding whether to install anything.
func (s *Store) AppliedSchemaVersion(ctx context.Context) (int64, error) {
	state, err := s.classifyMigrationState(ctx)
	if err != nil {
		return 0, err
	}
	return state.appliedVersion, nil
}

// adoptPreGooseWorkspace records the baseline version for a workspace already
// at the canonical shape, then removes the superseded legacy schema_version
// key so goose_db_version is the sole authority on "what's applied".
//
// [LAW:one-source-of-truth] After adoption, goose_db_version owns applied-state;
// the legacy meta.schema_version key is deleted so two authorities cannot
// coexist and drift.
//
// The key is dropped first, while no goose table exists: a failed drop or a
// failed version-table create leaves a workspace the next open classifies as
// pre-goose and adopts again, and only a failed stamp leaves goose state, an
// empty goose table. Stamping first would leave a failed drop's key in a
// workspace every later open reads as managed, and no open drops it.
func (s *Store) adoptPreGooseWorkspace(ctx context.Context, p snapshotted) *SchemaConvergenceError {
	if fail := p.attempt(stepAdoptDropLegacyKey, func() error {
		_, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE meta_key = 'schema_version'`)
		return err
	}); fail != nil {
		return fail
	}
	var store database.Store
	if fail := p.attempt(stepAdoptVersionTable, func() (err error) {
		store, err = database.NewStore(goose.DialectMySQL, gooseVersionTable)
		if err != nil {
			return fmt.Errorf("construct goose store: %w", err)
		}
		return store.CreateVersionTable(ctx, s.db)
	}); fail != nil {
		return fail
	}
	return p.attempt(stepAdoptStamp, func() error {
		return store.Insert(ctx, s.db, database.InsertRequest{Version: baselineVersion})
	})
}

// recordedMigrationVersion returns goose's recorded version, or 0 when none has
// been applied yet.
func (s *Store) recordedMigrationVersion(ctx context.Context) (int64, error) {
	store, err := database.NewStore(goose.DialectMySQL, gooseVersionTable)
	if err != nil {
		return 0, fmt.Errorf("construct goose store: %w", err)
	}
	version, err := store.GetLatestVersion(ctx, s.db)
	if err != nil {
		if errors.Is(err, database.ErrVersionNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("read recorded migration version: %w", err)
	}
	return version, nil
}

// verifyBaselineShape compares the live workspace against the baseline schema
// parsed from 00001_baseline.sql. It returns how many baseline tables are
// present and a list of every shape gap: a fully-absent table is reported as
// "<table>", a present table missing a column as "<table>.<column>".
//
// Checking column presence (not just table presence) is what makes "adoptable"
// mean "actually at baseline": a pre-goose workspace can carry every table yet
// still be pre-converged (e.g. issue_events.assignee never renamed to actor, or
// issues missing topic), and stamping such a workspace at v1 would permanently
// mark an incompatible schema as baseline.
//
// [LAW:one-source-of-truth] The expected shape is parsed from the same baseline
// file goose applies; there is no hand-maintained table/column list to drift.
// Column NAMES are compared (not types/constraints): identifiers survive Dolt's
// DDL round-trip verbatim, so name presence is a robust discriminator without
// the rewrite brittleness that full-text constraint matching suffers. Exact
// shape (types, constraints, indexes) is the drift canary's job (sxsk.4).
func (s *Store) verifyBaselineShape(ctx context.Context) (present int, missing []string, err error) {
	schema, err := baselineSchema()
	if err != nil {
		return 0, nil, err
	}
	for _, table := range sortedKeys(schema) {
		actual, err := s.tableColumns(ctx, table)
		if err != nil {
			return 0, nil, err
		}
		if len(actual) == 0 {
			missing = append(missing, table)
			continue
		}
		present++
		for _, column := range schema[table] {
			if !actual[column] {
				missing = append(missing, table+"."+column)
			}
		}
	}
	return present, missing, nil
}

// tableColumns returns the set of column names a table has in the active
// database. An absent table yields an empty set.
func (s *Store) tableColumns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?`,
		table,
	)
	if err != nil {
		return nil, fmt.Errorf("probe columns of %q: %w", table, err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan column of %q: %w", table, err)
		}
		columns[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate columns of %q: %w", table, err)
	}
	return columns, nil
}

// tableExists reports whether a table is present in the active database.
func (s *Store) tableExists(ctx context.Context, table string) (bool, error) {
	var marker int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ? LIMIT 1`,
		table,
	).Scan(&marker)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("probe table %q: %w", table, err)
}

// newGooseProvider builds a goose provider over the embedded registry. mysql
// dialect: Dolt speaks the MySQL protocol.
func newGooseProvider(db *sql.DB) (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectMySQL, db, migrations.FS)
}

// migrationCommitMessage is the one-line Dolt commit message for an applied
// migration: `migrate: v<N> <file>`.
func migrationCommitMessage(result *goose.MigrationResult) string {
	return fmt.Sprintf("migrate: v%d %s", result.Source.Version, filepath.Base(result.Source.Path))
}

// baselineSchema parses the embedded baseline migration into the table->columns
// shape it creates — the single oracle for what "at baseline" means. The same
// file goose applies on a fresh workspace defines what adoption must verify on
// a pre-goose one.
func baselineSchema() (map[string][]string, error) {
	name, err := migrations.BaselineFileName()
	if err != nil {
		return nil, err
	}
	data, err := migrations.FS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read baseline migration %q: %w", name, err)
	}
	schema := parseCreateTableColumns(gooseUpSection(string(data)))
	if len(schema) == 0 {
		return nil, fmt.Errorf("baseline migration %q defines no tables", name)
	}
	return schema, nil
}

// sortedKeys returns the map keys in deterministic order so adoption probing and
// error messages are stable across runs.
func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
