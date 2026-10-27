package store

import (
	"context"
	"fmt"
	"os"
	"slices"
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
// is swept at the start of the next batch, and recorded as an orphan; a sweep
// that cannot list them fails the batch, as any read before it writes does.
//
// It returns what of fn's result stands beside the batch's landing: all of
// the result when the batch landed, which a landed batch whose scratch
// cleanup failed returns beside that failure; the zero result beside a
// *storage.RefusedBatchError when fn refused it, or beside the landing's
// error when the batch could not land. The observers hear the batch's changes
// (notify queues them) only when it landed, once the lock is released, as a
// lone Apply's observers do. [LAW:no-silent-failure] [LAW:nothing-unseen]
func withBatch[R any](ctx context.Context, s *Store, fn func(ctx context.Context, result *R) error) (R, batchLanding, error) {
	var built, stands R
	var heard []heardChange
	var landing batchLanding
	err := s.withCommitLock(ctx, func(ctx context.Context) error {
		var from string
		var orphans []string
		// Retried as every write is, since nothing is written yet: a re-run
		// finds the stranded writes already committed and reads the same head.
		if err := s.retryInPlace(ctx, func(ctx context.Context) error {
			var err error
			if from, err = activeBranch(ctx, s.db); err != nil {
				return err
			}
			// A batch lands on a branch that holds data, so it refuses a
			// scratch branch before it writes anything there: a scratch branch
			// is deleted when its run ends, and the sweep below would delete
			// another batch's live branch.
			if slices.ContainsFunc(scratchPrefixes, func(prefix string) bool { return strings.HasPrefix(from, prefix) }) {
				return fmt.Errorf("refusing to start a batch on %q, a scratch branch", from)
			}
			if err := s.commitStrandedWrites(ctx); err != nil {
				return err
			}
			if landing.From, err = readDoltHead(ctx, s.db); err != nil {
				return err
			}
			orphans, err = s.branchesUnderPrefix(ctx, batchScratchPrefix)
			return err
		}); err != nil {
			return fmt.Errorf("batch: %w", err)
		}
		// A leftover branch is never from and is never pushed, so a delete
		// that fails is reported, on the event and here, and the batch goes
		// on. [LAW:no-silent-failure]
		if err := s.deleteScratch(ctx, orphans, ""); err != nil {
			fmt.Fprintf(os.Stderr, "lit: a batch could not delete the branches killed batches left: %v\n", err)
		}
		scratch := fmt.Sprintf("%s%d-%d", batchScratchPrefix, os.Getpid(), time.Now().UnixNano())
		err := s.runOnScratch(ctx, from, []string{scratch}, landing.From, s.retryInPlace, func() error {
			if cause := fn(context.WithValue(ctx, batchHearingKey{}, &heard), &built); cause != nil {
				return &storage.RefusedBatchError{Head: landing.From, Cause: cause}
			}
			if err := s.landScratch(ctx, from, scratch, s.retryInPlace); err != nil {
				return fmt.Errorf("batch: land %s on %s: %w", scratch, from, err)
			}
			landing.Landed = true
			return nil
		})
		// What stands is whether from moved, not how the run ended: a
		// landed batch whose scratch cleanup failed still stands whole.
		if landing.Landed {
			stands = built
		} else {
			heard = nil
		}
		return err
	})
	s.hear(ctx, heard...)
	return stands, landing, err
}

// batchLanding is where a batch started and whether it landed: From is the
// head the batch started from, which the commits a landed batch wrote sit
// directly above, as undo's commitsAbove reads them.
type batchLanding struct {
	From   string
	Landed bool
}
