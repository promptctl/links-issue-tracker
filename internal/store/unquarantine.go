package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Unquarantined is what Unquarantine did: the quarantine it cleared, whose
// Next is the one the next open still meets. A migration runs again on the
// next open only when Next is nil, so the outcome carries the fact that
// decides what may be said about it. [LAW:types-are-the-program]
type Unquarantined struct {
	Cleared QuarantineBlockError
}

// String is the clear and what the next open meets.
func (u Unquarantined) String() string { return clearedReport(u.Cleared) }

// EventRecord is the version cleared and, when one remains, the version the
// next open still refuses on. [LAW:nothing-unseen]
func (u Unquarantined) EventRecord() (map[string]any, map[string]int) {
	return clearedFacts(u.Cleared), nil
}

// UnquarantineCommitError is a clear whose DELETE landed and whose commit did
// not. The clear is in force: the working set is what the next open reads, and
// the next commit versions it, as an uncommitted quarantine row is in force in
// handleMigrationFailure.
type UnquarantineCommitError struct {
	Cleared QuarantineBlockError
	Cause   error
}

func (e *UnquarantineCommitError) Error() string {
	return fmt.Sprintf("the clear is in force, but its commit did not land, so the next write commits it: %v\n%s", e.Cause, clearedReport(e.Cleared))
}

func (e *UnquarantineCommitError) Unwrap() error { return e.Cause }

// EventRecord is the same facts a clear that committed records: the clear is
// in force either way. [LAW:one-source-of-truth]
func (e *UnquarantineCommitError) EventRecord() (map[string]any, map[string]int) {
	return clearedFacts(e.Cleared), nil
}

// clearedReport is the clear and what the next open meets: the cleared
// migration run again, or the quarantine still in force refusing it first.
func clearedReport(cleared QuarantineBlockError) string {
	next := "the next command that opens the store runs it again"
	if cleared.Next != nil {
		next = fmt.Sprintf("migration v%d %q is still quarantined, so the next command that opens the store refuses on it; `lit unquarantine` clears it", cleared.Next.Version, cleared.Next.Name)
	}
	return fmt.Sprintf("cleared the quarantine of migration v%d %q\n%s", cleared.Version, cleared.Name, next)
}

func clearedFacts(cleared QuarantineBlockError) map[string]any {
	facts := map[string]any{"unquarantine.cleared_version": cleared.Version}
	if cleared.Next != nil {
		facts["unquarantine.next_version"] = cleared.Next.Version
	}
	return facts
}

// NoQuarantineError answers an Unquarantine on a workspace no quarantine
// blocks: the state asked for already holds.
type NoQuarantineError struct{}

func (NoQuarantineError) Error() string {
	return "no migration is quarantined: no open of this workspace is refused, so there is nothing to clear"
}

// Satisfied makes the answer a state request that already holds.
func (NoQuarantineError) Satisfied() bool { return true }

var (
	_ model.StateRequestError = NoQuarantineError{}
	_ model.RecordedError     = (*UnquarantineCommitError)(nil)
)

// Unquarantine clears the quarantine every write open of this workspace is
// refused on, so the next open runs that migration again, and commits the
// clear as one Dolt commit.
//
// [LAW:locality-or-seam] This is a below-the-gate entry point like DumpRaw:
// Open's migrate is exactly what refuses a quarantined workspace, so this
// opens through openInitialized and never migrates.
func Unquarantine(ctx context.Context, doltRootDir string, workspaceID string) (_ Unquarantined, err error) {
	s, err := openInitialized(ctx, doltRootDir, workspaceID, engineWrite)
	if err != nil {
		return Unquarantined{}, err
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	return s.unquarantine(ctx)
}

// unquarantine reads which quarantine blocks, once, and clears it. Nothing is
// read after the commit: what the next open meets is the cleared quarantine's
// Next, read before the DELETE.
//
// [LAW:single-enforcer] Which quarantine blocks is answered by quarantineGate,
// the gate migrate refuses on, under the same willMutate condition migrate
// consults it under. It takes no version argument, so it cannot clear a row
// the gate would not refuse on.
func (s *Store) unquarantine(ctx context.Context) (Unquarantined, error) {
	var done Unquarantined
	err := s.withCommitLock(ctx, func(ctx context.Context) error {
		cleared, err := s.blockingQuarantine(ctx)
		if err != nil {
			return err
		}
		if cleared == nil {
			return NoQuarantineError{}
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM migration_quarantine WHERE version = ?`, cleared.Version); err != nil {
			return fmt.Errorf("clear quarantine of v%d: %w", cleared.Version, err)
		}
		if err := s.commitWorkingSet(ctx, fmt.Sprintf("unquarantine v%d %s", cleared.Version, cleared.Name)); err != nil {
			return &UnquarantineCommitError{Cleared: *cleared, Cause: err}
		}
		done = Unquarantined{Cleared: *cleared}
		return nil
	})
	return done, err
}

// blockingQuarantine is the quarantine the next write open is refused on, nil
// when none. An open with nothing to migrate never reaches the gate, so
// nothing blocks it.
func (s *Store) blockingQuarantine(ctx context.Context) (*QuarantineBlockError, error) {
	state, _, fail := s.checkWritable(ctx)
	if fail != nil {
		return nil, fail
	}
	if !state.willMutate() {
		return nil, nil
	}
	fail = s.quarantineGate(ctx, state)
	if fail == nil {
		return nil, nil
	}
	var blocked *QuarantineBlockError
	if errors.As(fail, &blocked) {
		return blocked, nil
	}
	return nil, fail
}
