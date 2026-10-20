package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// SchemaConvergenceError is every failure of an open's schema convergence:
// the step that failed, the recovery snapshot the open took before it, the
// prunes that failed before it, and why the step failed. Every open that
// converges the schema (the write open's migrate, the read open's assessment,
// the sync open's floor) reports its failure as this one type, so a caller
// reads what clears the failure off the step instead of off whichever exit
// fired. [LAW:single-enforcer] [LAW:one-type-per-behavior]
type SchemaConvergenceError struct {
	Step ConvergenceStep
	// Snapshot is the recovery snapshot this open took before migrating, nil
	// when the open failed before taking one.
	Snapshot *dbsnapshot.Snapshot
	// PruneErr is why rolling older migration checkpoints or snapshots off
	// failed, during this open, before or beside the failure.
	PruneErr error
	Cause    error
}

// Error names the step and its cause, and states the snapshot and the prune
// failure as facts. Whether to restore the snapshot is the step's act to say,
// so no restore instruction is printed here.
// [LAW:one-source-of-truth] the act decides the advice; the message only
// says what exists.
func (e *SchemaConvergenceError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "migrate: step %s failed: %v", e.Step, e.Cause)
	if e.Snapshot != nil {
		fmt.Fprintf(&b, "\n\nthis open took recovery snapshot %s before migrating; it holds the workspace as it was then:\n  %s",
			e.Snapshot.Name, e.Snapshot.Path)
	}
	if e.PruneErr != nil {
		fmt.Fprintf(&b, "\n\nremoving old migration checkpoints or snapshots also failed: %v", e.PruneErr)
	}
	return b.String()
}

func (e *SchemaConvergenceError) Unwrap() error { return e.Cause }

// EventRecord is the step that failed, and the snapshot and prune failure
// under the keys a successful open records them under.
func (e *SchemaConvergenceError) EventRecord() (map[string]any, map[string]int) {
	snapshot := ""
	if e.Snapshot != nil {
		snapshot = e.Snapshot.Name
	}
	facts := storage.MigrationRecoveryFacts(snapshot, e.PruneErr)
	facts["migration.failed_step"] = e.Step.String()
	return facts, nil
}

var _ model.RecordedError = (*SchemaConvergenceError)(nil)

// ConvergenceStep is one step of an open's schema convergence whose failure
// fails the open. A step is one operation of the convergence's sequence; an
// operation whose exits leave different states is split, so each step has
// exactly one act.
// [LAW:types-are-the-program] a closed enum: a failure cannot name a step
// that does not exist, and a step cannot exist without an act.
type ConvergenceStep int

const (
	stepClassify ConvergenceStep = iota
	stepVerifyContent
	stepRefuseUnwritable
	stepDriftCheckpoint
	stepDriftRepair
	stepDriftRepairReset
	stepEnsureQuarantineTable
	stepQuarantineTableShape
	stepCommitQuarantineTable
	stepCheckQuarantine
	stepCheckReconcilePrerequisites
	stepReconcilePrerequisitesMissing
	stepSnapshot
	stepReconcile
	stepVerifyReconciledShape
	stepReconciledShapeDiffers
	stepAdoptVersionTable
	stepAdoptStamp
	stepAdoptDropLegacyKey
	stepCommitAdoptionStamp
	stepMigrationProvider
	stepMigrationCheckpoint
	stepMigration
	stepCommitMigration
	stepMigrationReset
	stepCommitMigrationReset
	stepRecordQuarantine
	stepCommitQuarantine
	convergenceStepCount
)

// ConvergenceAct is what clears a failed step, decided by the state the step
// leaves behind.
type ConvergenceAct int

const (
	// The zero act is no act, so a step row that forgot its act is caught.
	_ ConvergenceAct = iota
	// ActClearCause: a retry converges from whatever state the step left, so
	// what clears the cause clears the failure.
	ActClearCause
	// ActRestoreSnapshot: the working set may hold partial changes the next
	// open would build on, so the snapshot this open took is restored first.
	ActRestoreSnapshot
	// ActEscalate: the stored shape is one the open does not converge, and no
	// lit command clears it.
	ActEscalate
)

// convergenceSteps is the one place a step's token and act are declared.
// Adding a failure exit to the convergence is one step value and one row here.
// [LAW:one-source-of-truth]
var convergenceSteps = [convergenceStepCount]struct {
	token string
	act   ConvergenceAct
}{
	stepClassify:         {"classify", ActClearCause},
	stepVerifyContent:    {"verify_content", ActClearCause},
	stepRefuseUnwritable: {"refuse_unwritable", ActClearCause},
	stepDriftCheckpoint:  {"drift_checkpoint", ActClearCause},
	stepDriftRepair:      {"drift_repair", ActClearCause},
	// The next open probes the live schema and repairs whatever is still
	// missing, whether or not the reset held.
	stepDriftRepairReset:      {"drift_repair_reset", ActClearCause},
	stepEnsureQuarantineTable: {"ensure_quarantine_table", ActClearCause},
	stepQuarantineTableShape:  {"quarantine_table_shape", ActEscalate},
	stepCommitQuarantineTable: {"commit_quarantine_table", ActClearCause},
	// A quarantine in force is carried by the cause, QuarantineBlockError.
	stepCheckQuarantine:               {"check_quarantine", ActClearCause},
	stepCheckReconcilePrerequisites:   {"check_reconcile_prerequisites", ActClearCause},
	stepReconcilePrerequisitesMissing: {"reconcile_prerequisites_missing", ActEscalate},
	stepSnapshot:                      {"snapshot", ActClearCause},
	// Probe-driven and idempotent: the next open's adopt runs it again.
	stepReconcile:             {"reconcile", ActClearCause},
	stepVerifyReconciledShape: {"verify_reconciled_shape", ActClearCause},
	// Restoring the snapshot gives back the shape that fails.
	stepReconciledShapeDiffers: {"reconciled_shape_differs", ActEscalate},
	stepAdoptVersionTable:      {"adopt_version_table", ActClearCause},
	// An empty goose table: the next open classifies the workspace as managed
	// at v0 and applies the baseline on top of the existing tables.
	stepAdoptStamp: {"adopt_stamp", ActRestoreSnapshot},
	// The working set is stamped; the next open reads it as managed at the
	// baseline, and its next commit includes the adoption.
	stepAdoptDropLegacyKey:  {"adopt_drop_legacy_key", ActClearCause},
	stepCommitAdoptionStamp: {"commit_adoption_stamp", ActClearCause},
	stepMigrationProvider:   {"migration_provider", ActClearCause},
	stepMigrationCheckpoint: {"migration_checkpoint", ActClearCause},
	// The reset held; the cause is the CheckpointResetError, which carries a
	// quarantine in force.
	stepMigration:       {"migration", ActClearCause},
	stepCommitMigration: {"commit_migration", ActClearCause},
	// The working set may hold the failed goose step's partial changes, and
	// every later open runs the step again on top of them.
	stepMigrationReset: {"migration_reset", ActRestoreSnapshot},
	// goose applied and recorded the step before its commit failed, so the
	// working set holds a complete step the next open reads as applied.
	stepCommitMigrationReset: {"commit_migration_reset", ActClearCause},
	stepRecordQuarantine:     {"record_quarantine", ActClearCause},
	// The uncommitted quarantine row stays in force, and the cause carries it.
	stepCommitQuarantine: {"commit_quarantine", ActClearCause},
}

func (s ConvergenceStep) String() string { return convergenceSteps[s].token }

// Act is what clears this step's failure.
func (s ConvergenceStep) Act() ConvergenceAct { return convergenceSteps[s].act }

// ConvergenceSteps is every step, in the convergence's order.
func ConvergenceSteps() []ConvergenceStep {
	steps := make([]ConvergenceStep, convergenceStepCount)
	for i := range steps {
		steps[i] = ConvergenceStep(i)
	}
	return steps
}

// pruneStep is a prune that runs after its work committed. Its failure is
// reported on the open's outcome and never fails the open, so it is not a
// ConvergenceStep and no SchemaConvergenceError can carry one.
type pruneStep int

const (
	pruneMigrationCheckpoints pruneStep = iota
	pruneDriftCheckpoints
	pruneMigrationSnapshots
)

var pruneStepTokens = [...]string{
	pruneMigrationCheckpoints: "migration_checkpoint_prune",
	pruneDriftCheckpoints:     "drift_checkpoint_prune",
	pruneMigrationSnapshots:   "snapshot_prune",
}

func (p pruneStep) String() string { return pruneStepTokens[p] }

// failpoint is a named place in the convergence where a test can inject a
// failure: every ConvergenceStep and every pruneStep. Sealed by its unexported
// method, so a test can name only a step that exists.
type failpoint interface {
	fmt.Stringer
	isFailpoint()
}

func (ConvergenceStep) isFailpoint() {}
func (pruneStep) isFailpoint()       {}

// failpointForTest, if non-nil, is consulted before each step and prune of the
// convergence runs: an error it returns fails that step in place of its work.
var failpointForTest func(at failpoint) error

// atFailpoint runs work, or the failure a test injected at this point.
func atFailpoint(at failpoint, work func() error) error {
	if hook := failpointForTest; hook != nil {
		if err := hook(at); err != nil {
			return err
		}
	}
	return work()
}

// attempt runs one step and returns its failure, nil when it succeeded. A
// callee that names a finer step for one of its own exits (a shape refusal
// inside a step whose other exits are reads) returns that failure, and it
// passes through unchanged.
func attempt(step ConvergenceStep, work func() error) *SchemaConvergenceError {
	return failAt(step, atFailpoint(step, work))
}

// failAt is err as step's failure, nil when err is nil.
func failAt(step ConvergenceStep, err error) *SchemaConvergenceError {
	if err == nil {
		return nil
	}
	var named *SchemaConvergenceError
	if errors.As(err, &named) {
		return named
	}
	return &SchemaConvergenceError{Step: step, Cause: err}
}

// prune runs one prune and returns its failure, labeled, nil when it
// succeeded.
func prune(at pruneStep, label string, work func() error) error {
	if err := atFailpoint(at, work); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// snapshotted builds the failures of the steps that run after the open took
// its recovery snapshot, from that snapshot.
// [LAW:types-are-the-program] the snapshot is a value this phase receives,
// not a field attached after the fact, so its failures carry it by
// construction.
type snapshotted struct{ snap dbsnapshot.Snapshot }

func (p snapshotted) attempt(step ConvergenceStep, work func() error) *SchemaConvergenceError {
	return p.failAt(step, atFailpoint(step, work))
}

func (p snapshotted) failAt(step ConvergenceStep, err error) *SchemaConvergenceError {
	fail := failAt(step, err)
	if fail != nil {
		fail.Snapshot = &p.snap
	}
	return fail
}
