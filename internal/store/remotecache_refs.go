package store

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Dolt's git blobstore gives each instance it opens on a mirror two refs named
// with the instance's own UUID — a tracking ref under refs/dolt/remotes/ and a
// local ref under refs/dolt/blobstore/ (RemoteTrackingRef and OwnedLocalRef in
// the fork's store/blobstore/git_refs.go) — and deletes both when the instance
// closes, after publishing its tracking ref to one shared last-known ref (fork
// patch 7, FORKS.md). Every close publishes, so the last-known ref holds what
// the last instance to close saw. So between operations a mirror holds exactly
// its last-known refs, and an instance ref on disk is one whose instance never
// closed: left by a lit built before patch 7, whose fetches never closed their
// blobstore, or by a process that died holding one.
//
// Each such ref pins the remote snapshot it last saw, so git gc can never
// release it. The sweep below is what collects them.

const (
	// lastKnownRefNamespace is where fork patch 7's LastKnownRemoteRef puts the
	// one ref a mirror keeps per remote between operations.
	lastKnownRefNamespace = "refs/dolt/last-known/"
	// trackingRefNamespace holds each instance's view of the remote
	// (RemoteTrackingRef): a state the remote really held.
	trackingRefNamespace = "refs/dolt/remotes/"
	// localRefNamespace holds each instance's local working ref
	// (OwnedLocalRef), which can name a commit the remote never received — a
	// push the remote refused, or a writer that died before pushing.
	localRefNamespace = "refs/dolt/blobstore/"
)

// instanceRefNamespaces are the two namespaces Dolt names per-instance refs in,
// each followed by `<remote>/<ref>/<uuid>`.
var instanceRefNamespaces = [...]string{trackingRefNamespace, localRefNamespace}

// instanceRef is one blobstore instance's ref, read off its name.
type instanceRef struct {
	mirrorRef
	// namespace is which of instanceRefNamespaces the name sits in.
	namespace string
	// lastKnown is the ref this instance would have published to had it
	// closed: the same `<remote>/<ref>` under lastKnownRefNamespace.
	lastKnown string
}

// parseInstanceRef reads a ref name as an instance ref, and reports false for
// anything else. A name that fails is not an instance ref this code is unsure
// about; it is not an instance ref at all, and so never one the sweep may
// delete. [LAW:parse-dont-validate] The UUID must be in exactly the canonical
// form uuid.NewString writes — uuid.Parse also accepts braced, URN and
// undashed spellings Dolt never produces — so the round trip is the test.
func parseInstanceRef(ref mirrorRef) (instanceRef, bool) {
	for _, namespace := range instanceRefNamespaces {
		rest, found := strings.CutPrefix(ref.name, namespace)
		if !found {
			continue
		}
		slash := strings.LastIndexByte(rest, '/')
		if slash <= 0 {
			return instanceRef{}, false
		}
		id, err := uuid.Parse(rest[slash+1:])
		if err != nil || id.String() != rest[slash+1:] {
			return instanceRef{}, false
		}
		return instanceRef{mirrorRef: ref, namespace: namespace, lastKnown: lastKnownRefNamespace + rest[:slash]}, true
	}
	return instanceRef{}, false
}

// refSweepOutcome is what one sweep of orphaned instance refs has to say for
// itself.
type refSweepOutcome remoteCacheCollection

// Report renders the sweep's line; see remoteCacheCollection.report.
func (o refSweepOutcome) Report() string {
	return remoteCacheCollection(o).report("remote-cache ref sweep", "orphaned ref", "orphaned refs")
}

// plus is this outcome followed by a later run's. SyncCompactAndPush's body can
// run twice under the GC-contention retry, and the second sweep finds nothing
// the first did not already remove, so replacing one outcome with the next
// would erase the refs that were really removed. [LAW:no-silent-failure]
func (o refSweepOutcome) plus(next refSweepOutcome) refSweepOutcome {
	problems := slices.DeleteFunc([]string{o.Problem, next.Problem}, func(p string) bool { return p == "" })
	return refSweepOutcome{
		Removed:   o.Removed + next.Removed,
		Reclaimed: o.Reclaimed + next.Reclaimed,
		Problem:   strings.Join(problems, "; "),
	}
}

// sweepOrphanedInstanceRefs removes every instance ref from the mirrors of this
// store's configured remotes and collects what they alone kept alive.
//
// It is safe only where no git blobstore is open on those mirrors in any
// process, and it owns no lock that makes that true — its caller does. Two facts
// do: every blobstore on a mirror belongs to an engine on this store, and a
// write engine holds Dolt's LOCK, which no second write engine shares; and
// within this process the engine has just been rotated, which closed every
// remote database the previous engine held. So at the one call site, between
// compaction's reconnect and the push, every instance ref on disk is an orphan.
// [LAW:no-ambient-temporal-coupling] the precondition is that call site's, and
// it is stated there too.
//
// A mirror of a remote that is no longer configured is left to the prune, which
// removes it whole after the push; collecting it first would repack a directory
// about to be deleted.
func (s *Store) sweepOrphanedInstanceRefs(ctx context.Context) refSweepOutcome {
	keys, err := s.remoteMirrorKeys(ctx)
	if err != nil {
		return refSweepOutcome{Problem: err.Error()}
	}
	base := s.remoteCacheBase()
	onDisk, err := listRemoteCacheKeys(base)
	if err != nil {
		return refSweepOutcome{Problem: err.Error()}
	}
	configured, _ := splitMirrors(keys, onDisk)

	var outcome refSweepOutcome
	var problems []string
	// Every mirror is attempted and a failure records itself, so one mirror
	// that cannot be swept does not keep the rest from being collected.
	// [LAW:dataflow-not-control-flow]
	for _, key := range configured {
		removed, reclaimed, err := sweepMirror(ctx, filepath.Join(base, key, "repo.git"))
		outcome.Removed += removed
		outcome.Reclaimed += reclaimed
		if err != nil {
			problems = append(problems, fmt.Sprintf("mirror %s: %v", key, err))
		}
	}
	outcome.Problem = strings.Join(problems, "; ")
	return outcome
}

// sweepMirror deletes every instance ref in one mirror and collects the objects
// only they reached, returning how many refs it removed and the bytes that
// freed.
//
// Before deleting, it makes sure the mirror keeps one ref per remote: a remote
// whose last-known ref is absent gets the newest of its tracking refs published
// there. That is the state a pre-patch-7 mirror is in — instance refs and
// nothing else — and deleting them all would leave it negotiating its next
// fetch with no haves, which downloads the whole remote snapshot again. Only a
// tracking ref is a candidate, because only it names a state the remote held;
// a local ref can name a commit the remote never received. A last-known ref
// already present is kept as it is: a closed instance published it, which is
// the same standing any orphan would have.
//
// The publish and the deletes are one `git update-ref --stdin` transaction, each
// conditioned on the value just listed, so the mirror is never seen with the
// orphans gone and no last-known ref in their place.
func sweepMirror(ctx context.Context, gitDir string) (removed int, reclaimed int64, err error) {
	// Newest first, so the first tracking ref seen for a remote is the one to
	// publish if it has no last-known ref.
	refs, err := listMirrorRefs(ctx, gitDir)
	if err != nil {
		return 0, 0, err
	}
	present := map[string]bool{}
	var orphans []instanceRef
	for _, listed := range refs {
		present[listed.name] = true
		if ref, ok := parseInstanceRef(listed); ok {
			orphans = append(orphans, ref)
		}
	}
	if len(orphans) == 0 {
		return 0, 0, nil
	}

	var transaction strings.Builder
	for _, ref := range orphans {
		if ref.namespace == trackingRefNamespace && !present[ref.lastKnown] {
			// `create` fails if the ref exists, and the whole transaction
			// with it, so nothing is deleted. A last-known ref appearing
			// since the listing means an instance closed on this mirror
			// mid-sweep, which the precondition rules out; when it is
			// broken, the orphans are no longer known to be orphans.
			fmt.Fprintf(&transaction, "create %s %s\n", ref.lastKnown, ref.object)
			present[ref.lastKnown] = true
		}
	}
	for _, ref := range orphans {
		fmt.Fprintf(&transaction, "delete %s %s\n", ref.name, ref.object)
	}

	before, err := dirSize(gitDir)
	if err != nil {
		return 0, 0, fmt.Errorf("measure: %w", err)
	}
	update := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "update-ref", "--stdin")
	update.Stdin = strings.NewReader(transaction.String())
	if out, err := update.CombinedOutput(); err != nil {
		return 0, 0, fmt.Errorf("delete %d orphaned ref(s): %w: %s", len(orphans), err, strings.TrimSpace(string(out)))
	}
	// The refs are gone from here on, so what follows can fail without
	// un-removing them; the count goes out with any error. --prune=now because
	// nothing else is writing this mirror (sweepOrphanedInstanceRefs says why),
	// so no object can be in flight toward a ref that does not exist yet.
	if out, err := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "gc", "--prune=now", "--quiet").CombinedOutput(); err != nil {
		return len(orphans), 0, fmt.Errorf("git gc after removing %d orphaned ref(s): %w: %s", len(orphans), err, strings.TrimSpace(string(out)))
	}
	after, err := dirSize(gitDir)
	if err != nil {
		return len(orphans), 0, fmt.Errorf("measure after git gc: %w", err)
	}
	return len(orphans), before - after, nil
}
