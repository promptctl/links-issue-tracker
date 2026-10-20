package store

import (
	"context"
	"errors"
	"fmt"
)

// Unquarantined is what Unquarantine did: the quarantine it cleared, and the
// one the next open still meets, nil when none. A migration runs again on the
// next open only when Next is nil, so the outcome carries the fact that
// decides what may be said about it. [LAW:types-are-the-program]
type Unquarantined struct {
	Cleared QuarantineBlockError
	Next    *QuarantineBlockError
}

// EventRecord is the version cleared and, when one remains, the version the
// next open still refuses on. [LAW:nothing-unseen]
func (u Unquarantined) EventRecord() (map[string]any, map[string]int) {
	facts := map[string]any{"unquarantine.cleared_version": u.Cleared.Version}
	if u.Next != nil {
		facts["unquarantine.next_version"] = u.Next.Version
	}
	return facts, nil
}

// NoQuarantineError answers an Unquarantine on a workspace no quarantine
// blocks: the state asked for already holds, so nothing was written.
type NoQuarantineError struct{}

func (NoQuarantineError) Error() string {
	return "no migration is quarantined: no open of this workspace is refused, so there is nothing to clear"
}

// Satisfied makes the answer a state request that already holds.
func (NoQuarantineError) Satisfied() bool { return true }

// Unquarantine clears the quarantine every write open of this workspace is
// refused on, so the next open runs that migration again, and commits the
// clear as one Dolt commit.
//
// [LAW:locality-or-seam] This is a below-the-gate entry point like DumpRaw:
// Open's migrate is exactly what refuses a quarantined workspace, so this
// opens through openInitialized and never migrates.
//
// [LAW:single-enforcer] Which quarantine blocks is answered by quarantineGate,
// the gate migrate refuses on, under the same willMutate condition migrate
// consults it under. It takes no version argument, so it cannot clear a row
// the gate would not refuse on.
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
	var done Unquarantined
	err = s.withCommitLock(ctx, func(ctx context.Context) error {
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
			return fmt.Errorf("commit clear of quarantine v%d: %w", cleared.Version, err)
		}
		next, err := s.blockingQuarantine(ctx)
		if err != nil {
			return fmt.Errorf("quarantine of v%d cleared and committed; reading what the next open meets: %w", cleared.Version, err)
		}
		done = Unquarantined{Cleared: *cleared, Next: next}
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
