package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// scratchPrefixes is every prefix a scratch branch is named under: a
// reconcile's pair, a history bound's re-root, and a batch's. A collection
// below a cut sweeps them all, since any one of them keeps history below the
// cut reachable through the GC. [LAW:one-source-of-truth]
var scratchPrefixes = []string{reconcileScratchPrefix + "-", reRootScratchPrefix, batchScratchPrefix}

// runOnScratch force-creates each of branches at commit, runs body on them,
// and — whatever body returns — returns the session to home and deletes them
// (leaveScratch). Everything body writes lands on a scratch branch, so home
// moves only if body moves it, which it does with one reset once its work is
// whole (landScratch): a reader of home sees the head before or the head
// after, never a part, and a process killed inside body leaves home where it
// found it and the scratch branches for the next sweep (deleteBranchesUnder).
// A batch (withBatch) and a reconcile (replayUnderGuard, the take-one
// resolver) both run through it. [LAW:single-enforcer] the scratch lifecycle
// is written once.
//
// Creating several branches is several statements, so the cleanup is armed
// BEFORE the first of them rather than after the last. Registered after, a
// failure creating the second branch would return past the defer and strand
// the first. [LAW:no-ambient-temporal-coupling] the cleanup covers whatever
// exists when it runs, so it never depends on how far creation got. It runs to
// the end on a context the caller cannot cancel: the deadline that cut body
// short must not also strand the session on a scratch branch. The branch a
// rotation resumes on is the run's only while it lasts, so the cleanup puts
// back what the session recorded before it.
func (s *Store) runOnScratch(ctx context.Context, home string, branches []string, commit string, body func() error) (err error) {
	// created names only the branches that exist, so cleanup deletes exactly
	// those and never warns about one that was never made.
	var created []string
	before := s.checkedOut
	defer func() {
		if cleanupErr := s.leaveScratch(context.WithoutCancel(ctx), home, created); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		s.checkedOut = before
	}()
	// -B recreates each branch if a prior attempt of this same run left it
	// behind, so a creation is retried as every write is: checkoutNewAt
	// records a branch only once it exists, so a rotation resumes on the last
	// one made and the retry makes this one again.
	for _, branch := range branches {
		if err := retryTransientGCContention(ctx, func(ctx context.Context) error {
			if s.createScratchHookForTest != nil {
				if err := s.createScratchHookForTest(); err != nil {
					return err
				}
			}
			return s.checkoutNewAt(ctx, branch, commit)
		}, s.reconnect); err != nil {
			return fmt.Errorf("create scratch branch %q: %w", branch, err)
		}
		created = append(created, branch)
	}
	return body()
}

// landScratch moves home to scratch's head with one hard reset, the one move
// of home a scratch run makes: home holds the head before or the head after,
// never a part. It is retried as every write is, and checkout records home, so
// a rotation resumes there. It runs on a context the caller cannot cancel: a
// reset that applied and then read the caller's cancel would report a landed
// run as not landed. [LAW:single-enforcer] a batch and a reconcile land
// through it. [LAW:no-ambient-temporal-coupling]
func (s *Store) landScratch(ctx context.Context, home, scratch string) error {
	return retryTransientGCContention(context.WithoutCancel(ctx), func(ctx context.Context) error {
		if s.landHookForTest != nil {
			if err := s.landHookForTest(); err != nil {
				return err
			}
		}
		head, err := commitHashOfRef(ctx, s.db, scratch)
		if err != nil {
			return err
		}
		if err := s.checkout(ctx, home); err != nil {
			return fmt.Errorf("switch to %q: %w", home, err)
		}
		return resetHardToRef(ctx, s.db, head)
	}, s.reconnect)
}

// leaveScratch returns the session to home and deletes the scratch branches.
// Whatever body did to home already stands before this runs, so the one thing
// it must still guarantee is that the session is not left on a scratch branch —
// otherwise a later use of this store would silently read and write the wrong
// branch. If the switch back fails, the connection IS stranded on scratch, so
// it is rotated once: checkout recorded home, so the fresh connection resumes
// on it (resumeCheckedOut), and the deletes run from there. This is the one rotation outside
// retryTransientGCContention's budget; it runs once per scratch run, after its
// body ended, so it adds at most one engine close and one open to the hold.
// Only if that rotation ALSO fails is the store genuinely unusable — an
// unrecoverable state the caller promotes to its error. A failed delete is
// recoverable (the next sweep deletes the branch, and no scratch branch is
// ever pushed), so it is recorded and printed but not promoted.
// [LAW:no-silent-failure] recoverable failures recover loudly; unrecoverable
// ones fail the operation.
func (s *Store) leaveScratch(ctx context.Context, home string, created []string) error {
	if err := s.checkout(ctx, home); err != nil {
		fmt.Fprintf(os.Stderr, "lit: could not return to branch %q from a scratch branch (%v); rotating connection to recover\n", home, err)
		if reconnectErr := s.reconnect(ctx); reconnectErr != nil {
			return fmt.Errorf("a scratch run left the store on its scratch branch and could not recover: checkout %q failed (%v); connection rotation failed: %w", home, err, reconnectErr)
		}
	}
	for _, branch := range created {
		if err := s.deleteBranch(ctx, branch, storage.ScratchFinished); err != nil {
			fmt.Fprintf(os.Stderr, "lit: could not delete scratch branch %q: %v\n", branch, err)
		}
	}
	return nil
}

// deleteBranch deletes the scratch branch name and records the delete, with
// why it was deleted and, if it failed, why it is still there. Every delete of
// a scratch branch runs through it, so the event's scratch.deletes is every
// one. [LAW:single-enforcer] [LAW:nothing-unseen]
func (s *Store) deleteBranch(ctx context.Context, name string, cause storage.ScratchCause) error {
	err := execProcedureDiscard(ctx, s.db, "DOLT_BRANCH", "-D", name)
	if err != nil {
		err = fmt.Errorf("delete branch %q: %w", name, err)
	}
	s.scratchLooked = true
	s.scratchDeletes = append(s.scratchDeletes, storage.ScratchDelete{Branch: name, Cause: cause, Err: err})
	return err
}

// deleteBranchesUnder deletes every branch whose name begins with prefix
// (branchesUnderPrefix), as deleteScratch does. A sweep that found none still
// looked.
func (s *Store) deleteBranchesUnder(ctx context.Context, prefix, own string) error {
	names, err := s.branchesUnderPrefix(ctx, prefix)
	if err != nil {
		return err
	}
	return s.deleteScratch(ctx, names, own)
}

// branchesUnderPrefix lists every branch whose name begins with prefix: the
// scratch branches a batch, a reconcile or a history bound writes and a killed
// run abandons. [LAW:one-source-of-truth] "under prefix" is refHeadsUnder's
// literal match, never LIKE's wildcards.
func (s *Store) branchesUnderPrefix(ctx context.Context, prefix string) ([]string, error) {
	under, err := refHeadsUnder(ctx, s.db, `SELECT name, hash FROM dolt_branches`, prefix)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(under))
	for rest := range under {
		names = append(names, prefix+rest)
	}
	slices.Sort(names)
	return names, nil
}

// deleteScratch deletes the scratch branches names, a sweep's list. own is the
// branch the caller's own run made, "" for a sweep whose caller made none: it
// is deleted as finished, and every other branch as an orphan, since a sweep
// holds the commit lock and no run that owns one is live. Every branch is
// tried, and every failure is returned, joined: what one costs is the
// caller's to judge, since a batch or a reconcile can run beside a leftover
// branch and the history bound cannot collect under one.
func (s *Store) deleteScratch(ctx context.Context, names []string, own string) error {
	s.scratchLooked = true
	var errs []error
	for _, name := range names {
		cause := storage.ScratchOrphan
		if name == own {
			cause = storage.ScratchFinished
		}
		errs = append(errs, s.deleteBranch(ctx, name, cause))
	}
	return errors.Join(errs...)
}

// ScratchDeletes is every scratch branch this store deleted or failed to
// delete since it opened, oldest first, and whether anything looked for one.
func (s *Store) ScratchDeletes() ([]storage.ScratchDelete, bool) {
	return slices.Clone(s.scratchDeletes), s.scratchLooked
}
