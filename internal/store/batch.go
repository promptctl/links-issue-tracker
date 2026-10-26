package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// batchScratchPrefix names the branch a batch writes on before it lands. A
// batch's branch lives only inside its commit-lock hold, so one found under
// this prefix at a batch's start was left by a process killed mid-batch.
const batchScratchPrefix = "lit-batch/"

// withBatch runs fn as one batch: the branch the session is on (from) moves
// once, by the whole batch, or not at all. [LAW:single-enforcer] the one owner
// of what a batch leaves in this engine, and of what it returns; an import
// file is written through it, so a file refused at any write, or a process
// killed partway, writes nothing.
//
// fn runs on a scratch branch made at from's head (runOnScratch), and every
// write it makes is its own Dolt commit there. When fn returns, the batch
// lands: from is reset to the scratch head, a fast-forward, since the batch
// holds the commit lock throughout and nothing else moves from. The reset is
// one root update, so a read engine, which reads from without the lock, sees
// the head before the batch or the head after it, never part of it; and the
// commits above the head the batch started from are exactly what it wrote, as
// undo's commitsAbove reads them. A refused batch never lands, so from never
// moved, and there is nothing to put back. [LAW:no-ambient-temporal-coupling]
//
// The whole batch runs under ONE commit-lock hold. The lock is re-entrant, so
// every write fn makes short-circuits its own acquisition. A write a previous
// process staged and never committed is committed first
// (commitStrandedWrites), so the stranded write stays from's and is never
// carried onto the batch's branch. A batch's branch that a killed process left
// is swept at the start of the next batch, and recorded as an orphan.
//
// It returns the head the batch started from beside what of fn's result
// stands: all of it when the batch landed; the zero result beside a
// *storage.RefusedBatchError when fn refused it, or beside the landing's error
// when the batch could not land. The observers hear the batch's changes
// (notify queues them) only when it landed, once the lock is released, as a
// lone Apply's observers do. [LAW:no-silent-failure] [LAW:nothing-unseen]
func withBatch[R any](ctx context.Context, s *Store, fn func(ctx context.Context, result *R) error) (R, string, error) {
	var built, stands R
	var heard []heardChange
	var from, head string
	err := s.withCommitLock(ctx, func(ctx context.Context) error {
		// Retried as every write is, since nothing is written yet: a re-run
		// finds the stranded writes already committed and reads the same head.
		if err := retryTransientGCContention(ctx, func(ctx context.Context) error {
			if err := s.commitStrandedWrites(ctx); err != nil {
				return err
			}
			var err error
			if from, err = activeBranch(ctx, s.db); err != nil {
				return err
			}
			head, err = readDoltHead(ctx, s.db)
			return err
		}, s.reconnect); err != nil {
			return fmt.Errorf("batch: %w", err)
		}
		// Nothing runs a batch inside a batch, and the sweep below would
		// delete the outer one's live branch.
		if strings.HasPrefix(from, batchScratchPrefix) {
			return fmt.Errorf("batch: refusing to start a batch on %q, another batch's branch", from)
		}
		// A leftover branch is never from and is never pushed, so a sweep
		// that fails is reported and the batch goes on. [LAW:no-silent-failure]
		if err := s.deleteBranchesUnder(ctx, batchScratchPrefix, ""); err != nil {
			fmt.Fprintf(os.Stderr, "lit: a batch could not sweep the branches killed batches left: %v\n", err)
		}
		scratch := fmt.Sprintf("%s%d-%d", batchScratchPrefix, os.Getpid(), time.Now().UnixNano())
		landed := false
		err := s.runOnScratch(ctx, from, []string{scratch}, head, func() error {
			if cause := fn(context.WithValue(ctx, batchHearingKey{}, &heard), &built); cause != nil {
				return &storage.RefusedBatchError{Head: head, Cause: cause}
			}
			// Retried as every write is: checkout recorded from, so a
			// rotation resumes there, and the reset lands on it.
			if err := retryTransientGCContention(ctx, func(ctx context.Context) error {
				if s.landHookForTest != nil {
					if err := s.landHookForTest(); err != nil {
						return err
					}
				}
				scratchHead, err := commitHashOfRef(ctx, s.db, scratch)
				if err != nil {
					return err
				}
				if err := s.checkout(ctx, from); err != nil {
					return fmt.Errorf("switch to %q: %w", from, err)
				}
				return resetHardToRef(ctx, s.db, scratchHead)
			}, s.reconnect); err != nil {
				return fmt.Errorf("batch: land %s on %s: %w", scratch, from, err)
			}
			landed = true
			return nil
		})
		// What stands is whether from moved, not how the run ended: a
		// landed batch whose scratch cleanup failed still stands whole.
		if landed {
			stands = built
		} else {
			heard = nil
		}
		return err
	})
	s.hear(ctx, heard...)
	return stands, head, err
}
