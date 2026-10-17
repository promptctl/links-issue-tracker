package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
)

// ReRootHoldBudget bounds a re-root's work under the live store's hold. The
// work is proportional to the history it walks: a 1,000-commit window over a
// 5,127-commit store rewrote in 130 ms (links-storage-3k6u.8ka), so the budget
// is two orders of magnitude above the largest window the bound keeps, and a
// re-root still running at it is a stalled one.
const ReRootHoldBudget = 15 * time.Second

// ErrSideLineAcrossCut marks a cut that some line of history goes around: a
// commit the branch reaches that neither descends from the cut nor is one of
// its ancestors, which is a side line that forked below the cut and merged
// back above it. Re-rooting there would hand that line a parent it never had
// and move the merge base of every merge it takes part in, so the cut is
// refused and the branch left where it was.
var ErrSideLineAcrossCut = errors.New("a side line forks below the cut and merges above it")

// ErrReRootLandedThenFailed marks a re-root whose branch move landed and that
// failed after it, in Dolt's read-back of the moved branch or in a hold's
// release: the branch is on the rewritten head, and the result returned with
// the error describes it.
var ErrReRootLandedThenFailed = errors.New("the branch moved to its rewritten head, then the re-root failed")

// reRootRef is the ref the parentless root is committed to and deleted from
// within one re-root. Dolt builds a commit with no parents only by committing
// to a ref that has no head, which is how it makes its own init commit
// (refs/internal/create); an internal ref keeps the step out of every
// branch listing.
var reRootRef = ref.NewInternalRef("reroot")

// ReRooted is what ReRoot did to the branch.
type ReRooted struct {
	// Cut is the commit the history was cut at.
	Cut string
	// Root is the parentless commit carrying Cut's root value and metadata.
	Root string
	// Head is the branch's head after the re-root.
	Head string
	// Rewritten maps every commit the branch keeps — Cut and each commit
	// after it — from its hash before the re-root to its hash after.
	Rewritten map[string]string
	// Dropped counts the commits the branch reached before the re-root and
	// reaches no longer: the ancestors of Cut.
	Dropped int
	// Elapsed is the re-root's work inside the hold, from the commit lock's
	// acquisition to the branch move.
	Elapsed time.Duration
}

// Kept counts the commits the branch keeps.
func (r ReRooted) Kept() int {
	return len(r.Rewritten)
}

// EventRecord is the re-root as event facts and counts.
func (r ReRooted) EventRecord() (map[string]any, map[string]int) {
	return map[string]any{
			"reroot.cut":        r.Cut,
			"reroot.root":       r.Root,
			"reroot.head":       r.Head,
			"reroot.elapsed_ms": r.Elapsed.Milliseconds(),
		}, map[string]int{
			"reroot.kept":    r.Kept(),
			"reroot.dropped": r.Dropped,
		}
}

// ReRoot bounds branch's history at cut, on the store at doltRootDir. It
// writes a parentless commit carrying cut's root value and commit metadata,
// then rewrites every commit after cut, oldest first, onto the rewritten
// commits of its parents, each with its own root value and metadata. A parent
// older than cut is dropped. The branch then moves to the rewritten head, and
// the tables at every kept commit are exactly what they were.
//
// Why it is deterministic: a Dolt commit's hash is a function of its root
// value, its ordered parents and its metadata, and every one of those is
// copied, never made — the metadata comes from the commit and not from
// datas.NewCommitMeta, which reads the clock. So two stores that hold the same
// history and re-root it at the same cut arrive at the same hashes without
// talking to each other, which is what lets every checkout sharing a remote
// rebuild the remote's new history from its own copy. Re-rooting at a commit
// that is already parentless rewrites every commit to itself.
//
// Why merge results are unchanged: cut must be on every line of history from
// the head back to the root (ErrSideLineAcrossCut is the refusal). Then every
// kept commit descends from cut, every merge base of two kept commits is cut
// or a descendant of it, and each such base keeps its place in the rewritten
// history. A commit whose parents are all older than cut can only be on a
// line that went around it.
//
// It contacts no network and opens no SQL engine; the hold is
// holdForRefWrite's, under ReRootHoldBudget. Deciding when to re-root, and
// where, is the caller's.
func ReRoot(ctx context.Context, doltRootDir, branch, cut string) (ReRooted, error) {
	root, err := validateDoltRootDir(doltRootDir)
	if err != nil {
		return ReRooted{}, err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return ReRooted{}, err
	}
	trimmedCut := strings.TrimSpace(cut)
	if !isDoltCommitHash(trimmedCut) {
		return ReRooted{}, fmt.Errorf("re-root %s: %q is not a Dolt commit hash", trimmedBranch, cut)
	}
	reRooted, err := reRootUnderHold(ctx, root, trimmedBranch, trimmedCut)
	return settleReRoot(reRooted, err, trimmedBranch, trimmedCut)
}

// reRootUnderHold is ReRoot's work under the live store's holds, both
// released before it returns. Its result's Head is set exactly when the
// branch moved, whatever the error.
func reRootUnderHold(ctx context.Context, root, branch, cut string) (reRooted ReRooted, err error) {
	release, err := acquireLiveStoreHold(ctx, root)
	if err != nil {
		return ReRooted{}, err
	}
	defer func() { err = errors.Join(err, release()) }()
	err = holdForRefWrite(ctx, root, ReRootHoldBudget, func(holdCtx context.Context, ddb *doltdb.DoltDB) error {
		start := time.Now()
		var rewriteErr error
		reRooted, rewriteErr = reRootBranch(holdCtx, ddb, ref.NewBranchRef(branch), hash.Parse(cut))
		reRooted.Elapsed = time.Since(start)
		return rewriteErr
	})
	return reRooted, err
}

// settleReRoot says what a re-root's error left standing. A move that landed
// stands even when the re-root failed after it: the result describes the
// move and the error wraps ErrReRootLandedThenFailed, as RecordPushedHead's
// ErrReceivedRefsNotRecorded does for its own landed write. Anything else is
// not known to have moved, and its result carries only the time it held, as
// LandFetchedHead's carries Held. [LAW:no-silent-failure]
func settleReRoot(r ReRooted, err error, branch, cut string) (ReRooted, error) {
	switch {
	case err == nil:
		return r, nil
	case r.Head == "":
		return r, fmt.Errorf("re-root %s at %s: %w", branch, cut, err)
	default:
		return r, fmt.Errorf("%w: re-root %s at %s moved it to %s: %w", ErrReRootLandedThenFailed, branch, cut, r.Head, err)
	}
}

// historyCommit is one commit of the branch's history as the re-root reads it.
type historyCommit struct {
	commit  *doltdb.Commit
	addr    hash.Hash
	height  uint64
	parents []hash.Hash
}

// reRootBranch is ReRoot's work inside the hold: read the history, plan the
// rewrite, write it, move the branch.
func reRootBranch(ctx context.Context, ddb *doltdb.DoltDB, branch ref.DoltRef, cut hash.Hash) (ReRooted, error) {
	headCommit, err := ddb.ResolveCommitRef(ctx, branch)
	if err != nil {
		return ReRooted{}, fmt.Errorf("resolve the branch head: %w", err)
	}
	head, err := headCommit.HashOf()
	if err != nil {
		return ReRooted{}, err
	}
	// Everything at or below the cut, then everything the head reaches above
	// it: the walk from the head stops wherever it meets the first set.
	below, err := readHistory(ctx, ddb, cut, nil)
	if err != nil {
		return ReRooted{}, err
	}
	above, err := readHistory(ctx, ddb, head, below)
	if err != nil {
		return ReRooted{}, err
	}
	steps, err := planReRoot(above, head, cut)
	if err != nil {
		return ReRooted{}, err
	}

	rewritten := map[hash.Hash]*doltdb.Commit{}
	rewritten[cut], err = commitRoot(ctx, ddb, below[cut].commit)
	if err != nil {
		return ReRooted{}, err
	}
	for _, step := range steps {
		parents := make([]*doltdb.Commit, len(step.parents))
		for i, p := range step.parents {
			parents[i] = rewritten[p]
		}
		rewritten[step.commit.addr], err = recommit(ctx, ddb, step.commit.commit, parents)
		if err != nil {
			return ReRooted{}, err
		}
	}
	// The result is read before the move, so nothing can fail after the
	// branch has moved but the hold's release.
	result := ReRooted{Cut: cut.String(), Rewritten: make(map[string]string, len(rewritten)), Dropped: len(below) - 1}
	for old, nc := range rewritten {
		addr, err := nc.HashOf()
		if err != nil {
			return ReRooted{}, err
		}
		result.Rewritten[old.String()] = addr.String()
	}
	result.Root = result.Rewritten[result.Cut]
	result.Head = result.Rewritten[head.String()]
	if err := ddb.SetHeadToCommit(ctx, branch, rewritten[head]); err != nil {
		return whereTheMoveLeft(ctx, ddb, branch, result, err)
	}
	return result, nil
}

// whereTheMoveLeft reads where a branch move that returned moveErr left the
// branch. Dolt reads the dataset back after its manifest lands, so the error
// does not say the branch stayed: the branch says. On the rewritten head, the
// result stands beside the error; anywhere else, or unreadable, it does not.
// The read runs past the hold's budget, since a cut is one way to get here.
func whereTheMoveLeft(ctx context.Context, ddb *doltdb.DoltDB, branch ref.DoltRef, result ReRooted, moveErr error) (ReRooted, error) {
	err := fmt.Errorf("move the branch to the rewritten head %s: %w", result.Head, moveErr)
	at, readErr := ddb.ResolveCommitRef(context.WithoutCancel(ctx), branch)
	if readErr != nil {
		return ReRooted{}, errors.Join(err, fmt.Errorf("read where the branch is: %w", readErr))
	}
	if addr, _ := at.HashOf(); addr.String() == result.Head {
		return result, err
	}
	return ReRooted{}, err
}

// rewriteStep is one commit after the cut and the parents it keeps, by their
// hashes before the re-root, in the commit's own parent order.
type rewriteStep struct {
	commit  historyCommit
	parents []hash.Hash
}

// planReRoot orders the commits above the cut parents-first and gives each
// the parents it keeps: those that are the cut or come after it. A parent
// older than the cut is dropped. A commit left with no parent is on a line
// that goes around the cut, and the plan is refused before anything is
// written.
func planReRoot(above map[hash.Hash]historyCommit, head, cut hash.Hash) ([]rewriteStep, error) {
	// The cut is on the branch when it is the head or some commit above it
	// names it as a parent.
	onBranch := head == cut
	for _, c := range above {
		onBranch = onBranch || slices.Contains(c.parents, cut)
	}
	if !onBranch {
		return nil, fmt.Errorf("the cut is not an ancestor of the branch head %s", head)
	}
	// Parents before children: a commit's height exceeds every parent's. How
	// commits of one height are ordered is free; the hashes written do not
	// depend on it.
	order := slices.SortedFunc(maps.Values(above), func(a, b historyCommit) int {
		return cmp.Compare(a.height, b.height)
	})
	kept := map[hash.Hash]bool{cut: true}
	steps := make([]rewriteStep, 0, len(order))
	for _, c := range order {
		var parents []hash.Hash
		for _, p := range c.parents {
			if kept[p] {
				parents = append(parents, p)
			}
		}
		if len(parents) == 0 {
			return nil, fmt.Errorf("%w: %s reaches the history below the cut without passing through it", ErrSideLineAcrossCut, c.addr)
		}
		kept[c.addr] = true
		steps = append(steps, rewriteStep{commit: c, parents: parents})
	}
	return steps, nil
}

// readHistory reads every commit from reaches, stopping at the commits in
// stop. A ghost commit is refused: its root value and metadata are not here
// to copy.
func readHistory(ctx context.Context, ddb *doltdb.DoltDB, from hash.Hash, stop map[hash.Hash]historyCommit) (map[hash.Hash]historyCommit, error) {
	read := map[hash.Hash]historyCommit{}
	pending := []hash.Hash{from}
	for len(pending) > 0 {
		addr := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, seen := read[addr]; seen {
			continue
		}
		if _, stopped := stop[addr]; stopped {
			continue
		}
		commit, err := readFullCommit(ctx, ddb, addr)
		if err != nil {
			return nil, fmt.Errorf("read commit %s: %w", addr, err)
		}
		height, err := commit.Height()
		if err != nil {
			return nil, err
		}
		parents, err := commit.ParentHashes(ctx)
		if err != nil {
			return nil, err
		}
		read[addr] = historyCommit{commit: commit, addr: addr, height: height, parents: parents}
		pending = append(pending, parents...)
	}
	return read, nil
}

// commitRoot writes the parentless copy of c: committed to reRootRef, which
// has no head, and the ref deleted again. A reRootRef left by a re-root that
// died between the two is deleted first, since committing onto it would make
// its head the root's parent.
func commitRoot(ctx context.Context, ddb *doltdb.DoltDB, c *doltdb.Commit) (*doltdb.Commit, error) {
	value, meta, err := commitContent(ctx, c)
	if err != nil {
		return nil, err
	}
	leftover, err := ddb.HasRef(ctx, reRootRef)
	if err != nil {
		return nil, err
	}
	if leftover {
		if err := ddb.DeleteBranch(ctx, reRootRef, nil); err != nil {
			return nil, fmt.Errorf("delete the leftover %s: %w", reRootRef, err)
		}
	}
	root, err := ddb.CommitWithParentCommits(ctx, value, reRootRef, nil, meta)
	if err != nil {
		return nil, fmt.Errorf("write the parentless root: %w", err)
	}
	if err := ddb.DeleteBranch(ctx, reRootRef, nil); err != nil {
		return nil, fmt.Errorf("delete %s: %w", reRootRef, err)
	}
	return root, nil
}

// recommit writes c again onto parents, as a commit no ref names yet.
func recommit(ctx context.Context, ddb *doltdb.DoltDB, c *doltdb.Commit, parents []*doltdb.Commit) (*doltdb.Commit, error) {
	value, meta, err := commitContent(ctx, c)
	if err != nil {
		return nil, err
	}
	nc, err := ddb.CommitDanglingWithParentCommits(ctx, value, parents, meta)
	if err != nil {
		return nil, fmt.Errorf("rewrite commit: %w", err)
	}
	return nc, nil
}

// commitContent is what a rewrite copies from c: its root value's address
// and its metadata, both verbatim.
func commitContent(ctx context.Context, c *doltdb.Commit) (hash.Hash, *datas.CommitMeta, error) {
	value, err := datas.GetCommitRootHash(c.Value())
	if err != nil {
		return hash.Hash{}, nil, err
	}
	meta, err := c.GetCommitMeta(ctx)
	if err != nil {
		return hash.Hash{}, nil, err
	}
	return value, meta, nil
}
