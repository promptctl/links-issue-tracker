package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Candidate is one disposable, fully isolated rebuild of a workspace: a fresh
// Dolt directory at the current baseline, loaded with the domain data a
// validated (dump, mapping) produced. It is the unit the recovery loop verifies
// (Doctor + conservation against the dump) and then either promotes or throws
// away.
//
// [LAW:types-are-the-program] A candidate OWNS one directory TREE. That is what
// makes "a rejected attempt leaves zero durable residue" a structural fact
// rather than a cleanup routine: discarding a candidate removes its root whole,
// and the next attempt is a different, empty tree — there are no rows to scrub
// and nothing one attempt can leak into the next. The alternative (reuse one
// workspace and roll back) would lean on the import path's row deletion to undo
// a rejected attempt, which is the drift-prone shape this type exists to forbid.
//
// The dolt workspace is nested one level INSIDE root, not placed at root itself,
// because Open's footprint is not just the dolt directory: it writes a workspace
// lock and migration snapshots as SIBLINGS of the dolt directory (in its
// parent). Rooting the candidate at the dolt dir's parent is what brings those
// siblings inside the owned tree, so one RemoveAll(root) is genuinely total.
type Candidate struct {
	store *Store
	root  string
	// expectedHead and workspaceID are the dump's provenance, carried so a
	// promotion can re-check that the live workspace is still at the commit this
	// candidate was rebuilt to replace (links-recovery-j0vl.7).
	// [LAW:one-source-of-truth] Both are stamped from the one dump when the
	// candidate is built, so "candidate built from dump A, promoted against the head
	// of dump B" is unrepresentable — the head and the rebuilt data cannot disagree
	// because they have a single origin.
	expectedHead string
	workspaceID  string
}

// ErrInvalidMapping marks a RebuildCandidate failure caused by the MAPPING — a
// malformed or inapplicable ShapeMapping the applier rejected — as distinct from
// an infrastructure failure (filesystem, store I/O) that can only occur once the
// mapping is known good. [LAW:types-are-the-program] The cause is carried in the
// error itself, so the recovery loop routes a mapping problem back as repair
// feedback while a build failure surfaces loudly — without re-deriving which kind
// it was. [LAW:no-silent-failure] A disk or store error must never be relabeled
// as mapping feedback and silently retried.
var ErrInvalidMapping = errors.New("mapping is not applicable to the dump")

// RebuildCandidate is the mechanical applier's lifecycle: it turns a validated
// (dump, mapping) into a fresh candidate workspace, or rejects. No LLM is in
// this path — deterministic and LLM mappers alike produce a ShapeMapping that
// flows through the identical Apply + load.
//
// [LAW:dataflow-not-control-flow] The sequence is the same on every attempt;
// the (dump, mapping) values are the only variability. Apply (pure) runs first,
// so an invalid or incomplete mapping is rejected before any directory or
// database handle exists — the common rejection cannot leave residue because no
// resource was acquired. Only once a valid Export exists does the lifecycle
// touch the filesystem.
//
// [LAW:one-source-of-truth] dump is read-only here and may be reused unchanged
// across attempts; Apply never mutates it, so two attempts from one dump yield
// identical candidates.
//
// parentDir is the directory under which the throwaway candidate directory is
// created (empty means the system temp dir). The caller owns where recovery
// scratch lives; the unique per-call subdirectory is what guarantees each
// attempt starts clean.
func RebuildCandidate(ctx context.Context, parentDir string, dump RawDump, mapping ShapeMapping) (_ *Candidate, err error) {
	// [LAW:single-enforcer] Apply folds through Validate — the one well-formedness
	// boundary — so a rejection here is exactly "the mapping is invalid/incomplete".
	export, err := Apply(dump, mapping)
	if err != nil {
		// [LAW:types-are-the-program] Tag mapping rejections so a caller can tell
		// "the mapping was bad" from "the rebuild failed for an infrastructure
		// reason"; everything below this point is filesystem/store I/O.
		return nil, fmt.Errorf("%w: %w", ErrInvalidMapping, err)
	}

	root, err := os.MkdirTemp(parentDir, "lit-candidate-*")
	if err != nil {
		return nil, fmt.Errorf("create candidate workspace dir: %w", err)
	}
	// [LAW:dataflow-not-control-flow] Cleanup runs unconditionally on the way out;
	// the success flag is the datum that decides whether it fires. A rejected
	// attempt thus removes the whole root tree (and closes the store, if opened)
	// it touched, leaving zero durable residue — the same idiom Open uses to
	// release resources it acquired before a failure.
	var st *Store
	success := false
	defer func() {
		if success {
			return
		}
		if st != nil {
			err = errors.Join(err, st.Close())
		}
		err = errors.Join(err, os.RemoveAll(root))
	}()

	// Open at a child of root so its sibling artifacts (workspace lock, migration
	// snapshots) land inside root rather than escaping into parentDir.
	st, err = Open(ctx, filepath.Join(root, "workspace"), dump.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("open candidate workspace: %w", err)
	}
	if err = st.ReplaceFromExport(ctx, export); err != nil {
		return nil, fmt.Errorf("load export into candidate: %w", err)
	}

	success = true
	return &Candidate{store: st, root: root, expectedHead: dump.DoltHead, workspaceID: dump.WorkspaceID}, nil
}

// Store hands out the built workspace so the verify gate can inspect it (Doctor,
// conservation Export against the dump). The candidate is the owner; the gate is
// a read-only consumer.
func (c *Candidate) Store() *Store { return c.store }

// detachForPromotion closes the candidate's store and surrenders its Dolt
// directory, transferring that directory out of the candidate's ownership so a
// promotion can rename it to the canonical path. The candidate still owns its
// root's scratch siblings (the workspace lock, migration snapshots), which a
// subsequent Discard removes; only the Dolt directory leaves. [LAW:types-are-the-program]
// Clearing store here makes a later Discard's store-close a no-op by its own
// state, so detach + Discard compose without a double-close.
func (c *Candidate) detachForPromotion() (string, error) {
	// [LAW:no-silent-failure] An empty root means the candidate was already
	// discarded (or is a zero value); filepath.Join("", "workspace") would hand
	// back a cwd-relative "workspace" path that a promotion would then rename into
	// the canonical location. Fail loudly rather than operate on an unintended
	// directory. (root is a value that explicitly represents ownership — Discard
	// clears it — so this is a real precondition, not a guard on an impossible
	// state.)
	if c.root == "" {
		return "", errors.New("candidate has no workspace to promote (already discarded or never built)")
	}
	doltDir := filepath.Join(c.root, "workspace")
	var err error
	if c.store != nil {
		err = c.store.Close()
		c.store = nil
	}
	return doltDir, err
}

// Discard releases the candidate's two resources: the open store handle and the
// on-disk root tree. [LAW:types-are-the-program] Each field's non-empty value IS
// "this resource is still held", so each is released against its own state — not
// against one shared flag. That difference is load-bearing: a store handle
// releases once, but the root must be retried until removal actually succeeds,
// or a transient filesystem error would strand the very residue this type exists
// to forbid. root is cleared only once RemoveAll succeeds, so a later Discard
// re-attempts cleanup. Idempotent: a caller may defer Discard and still discard
// explicitly on the reject path.
func (c *Candidate) Discard() error {
	var err error
	if c.store != nil {
		err = c.store.Close()
		c.store = nil
	}
	if c.root != "" {
		if rmErr := os.RemoveAll(c.root); rmErr != nil {
			return errors.Join(err, rmErr)
		}
		c.root = ""
	}
	return err
}
