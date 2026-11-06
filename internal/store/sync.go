package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltenv "github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/env/actions"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"golang.org/x/mod/semver"
)

const minEmbeddedDoltVersion = "v0.40.5-0.20260314011441-62975ef6bf36"
const minEmbeddedDriverVersion = "v0.2.1-0.20260314000741-0fe74e7ee31a"

// OpenSync opens the store for a sync session: a long hold (engineWrite) that
// applies no migration.
func OpenSync(ctx context.Context, env environ.Env, stderr io.Writer, doltRootDir string, workspaceID string) (*Store, error) {
	return openSync(ctx, env, stderr, doltRootDir, workspaceID, engineWrite)
}

// OpenMaintenance opens the store for the compaction backstop: a sync open
// whose hold is routine (engineWriteRoutine), since its caller runs the pass
// under a deadline and no writer behind it asked for it. DOLT_GC is engine work,
// so the hold heartbeats through it, and a writer waits out a pass however long
// a loaded machine stretches it: to that deadline, and through the disk work of
// a collection that had finished marking by then.
func OpenMaintenance(ctx context.Context, env environ.Env, stderr io.Writer, doltRootDir string, workspaceID string) (*Store, error) {
	return openSync(ctx, env, stderr, doltRootDir, workspaceID, engineWriteRoutine)
}

// openSync is the one sync open, for either hold.
func openSync(ctx context.Context, env environ.Env, stderr io.Writer, doltRootDir string, workspaceID string, access engineAccess) (_ *Store, err error) {
	// [LAW:single-enforcer] Route through the one argument-validation boundary
	// rather than re-inlining the same two checks, so OpenSync cannot drift from
	// the rest of the store's exported entry points on what an acceptable path or
	// workspace id is.
	if err := validateOpenArgs(doltRootDir, workspaceID); err != nil {
		return nil, err
	}
	if err := requireEmbeddedSyncSupport(); err != nil {
		return nil, err
	}
	// [LAW:single-enforcer] Workspace shared lock is acquired BEFORE
	// EnsureDatabase so the bootstrap and the long-lived sync connection
	// are both protected against a concurrent `lit snapshots restore`
	// rotating the Dolt directory — the same invariant store.Open enforces.
	release, err := acquireWorkspaceShared(ctx, stderr, doltRootDir)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if success {
			return
		}
		if relErr := release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	// Post-lock, same as Store.Open: a marker seen while holding the
	// workspace lock always belongs to a dead adopt (a live one holds the
	// lock exclusively), so this refusal can neither fire on a healthy
	// in-flight adopt nor miss a dead one that expired while we waited.
	// [LAW:no-ambient-temporal-coupling]
	if err = requireNoPendingAdopt(doltRootDir); err != nil {
		return nil, err
	}
	// [LAW:single-enforcer] Sync bootstrap reuses the Store database initializer so first-run sync and regular store opens share one creation boundary.
	bootstrap, err := ensureDoltDatabase(ctx, stderr, doltRootDir, workspaceID)
	if err != nil {
		return nil, err
	}
	// OpenSync is the on-change mirror's own engine open, so this is exactly
	// the call site that must wait out an earlier foreground command's (or
	// earlier mirror's) still-live engine instead of colliding with it — the
	// wait happens inside the eager engine open, on Dolt's own journal lock,
	// bounded by coResidentHolderWait.
	s, err := openStoreConnection(ctx, env, stderr, doltRootDir, workspaceID, access)
	if err != nil {
		return nil, err
	}
	s.releaseWorkspaceLock = release
	// The sync engine writes — merges, replays, resolutions, compactions — so
	// it meets the floor a write rule sets, as the migrating open does. It
	// applies no migration: a workspace behind this binary is the migrating
	// open's to bring forward. [LAW:single-enforcer] both write opens apply
	// the one floor through checkWritable, so a failure of either step is
	// the same SchemaConvergenceError in both.
	state, op, fail := s.checkWritable(ctx)
	if fail != nil {
		err = fail
	}
	s.migrated = storage.Migrated{Bootstrap: bootstrap, SchemaAhead: op.schemaAhead(), SchemaHold: state.schemaHold()}
	// Same per-open branch normalization Store.Open runs: the bootstrap only
	// normalizes at creation, so a pre-made database (an adopt's clone) gets
	// renamed to master here on the mirror's own engine. The decision is a
	// lock-free read and the commit lock is taken only when a rename is
	// actually due — read-only OpenSync consumers (lit sync status, every
	// mirror cycle) must not queue behind a snapshot copy's minutes-long
	// commit-lock hold for a no-op. The one branch is the domain's own
	// discriminator: masterRenameSource's typed absence.
	// [LAW:dataflow-not-control-flow]
	// [LAW:single-enforcer] The rename itself still runs only inside
	// ensureMasterDefaultBranch under the commit lock, which re-derives the
	// decision — the pre-check is an optimization, never the enforcer.
	var renameSource string
	if err == nil {
		renameSource, err = masterRenameSource(ctx, s.db)
	}
	if err == nil && renameSource != "" {
		err = s.withCommitLock(ctx, func(ctx context.Context) error {
			return ensureMasterDefaultBranch(ctx, s.db)
		})
	}
	if err != nil {
		err = wrapEngineOpenContention(err, doltRootDir)
		if closeErr := s.closeEngine(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		s.releaseWorkspaceLock = nil
		return nil, err
	}
	success = true
	return s, nil
}

func (s *Store) SyncListRemotes(ctx context.Context) ([]storage.SyncRemote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, url FROM dolt_remotes ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list dolt remotes: %w", err)
	}
	defer rows.Close()

	remotes := []storage.SyncRemote{}
	for rows.Next() {
		var remote storage.SyncRemote
		if err := rows.Scan(&remote.Name, &remote.URL); err != nil {
			return nil, fmt.Errorf("scan dolt remote: %w", err)
		}
		remotes = append(remotes, remote)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dolt remotes: %w", err)
	}
	return remotes, nil
}

// GitBackedRemoteURL translates a git remote URL (as reported by `git remote -v`)
// into the canonical Dolt git-backed transport URL (the `git+...` form). Every such
// URL is a git remote by construction, so the git-backed transport applies to all of
// them — https, ssh/scp, and local-path spellings alike — even when the URL omits the
// `.git` suffix that providers like GitHub legitimately allow.
//
// [LAW:one-source-of-truth] Dolt's NormalizeGitRemoteUrl is the single source of truth
// for the translation — it canonically handles scp, ssh, file, and local-path spellings
// (including the home-relative `/./` that a naive scp→ssh rewrite gets wrong). lit only
// supplies the one thing Dolt declines to recognize: a git remote whose `.git` suffix is
// absent. Dolt gates recognition on that suffix, so we append a synthetic one to run the
// canonical translator, then drop exactly what we added — leaving the transport URL
// pointed at the real, suffix-less remote path. The suffix is never the discriminator.
// [LAW:single-enforcer] Lives at the Store boundary, the one layer that owns the Dolt
// dependency, so every caller shares one transport contract instead of re-deriving it.
func GitBackedRemoteURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if normalized, ok, err := doltenv.NormalizeGitRemoteUrl(trimmed); ok && err == nil {
		return normalized
	}
	if normalized, ok, err := doltenv.NormalizeGitRemoteUrl(trimmed + ".git"); ok && err == nil {
		return strings.TrimSuffix(normalized, ".git")
	}
	return trimmed
}

func (s *Store) SyncAddRemote(ctx context.Context, name storage.RemoteName, url string) error {
	// [LAW:single-enforcer] The name arrives parsed (storage.RemoteName); the
	// url is required here, at the Store boundary, for every caller alike.
	trimmedURL, err := requireSyncArg("remote url", url)
	if err != nil {
		return err
	}
	return s.runSyncMutation(ctx, func(ctx context.Context) error {
		_, err := callIntProcedure(ctx, s.db, "DOLT_REMOTE", "add", name.String(), trimmedURL)
		if err != nil {
			return fmt.Errorf("add dolt remote %q: %w", name, err)
		}
		return nil
	})
}

// SyncRemoveRemote removes any remote Dolt holds, a name the parse refuses
// included: DOLT_CLONE --remote once wrote such names unchecked, and the
// reconcile must be able to remove them. The refs the removed name's fetches
// wrote belong to its owner (refOwner), so they go with it only when Dolt
// holds no other remote named that owner: for origin, origin's refs; for
// team/origin, team's, which are every team/* name's, and the reconcile
// removes each of those too; for a/b beside a, none, as they are a's
// branches b/*, kept until a's next fetch prunes them like any of a's stale
// branches. The drops go first and the remove last, and the remove runs only
// while Dolt holds the name, so an attempt retried after its remove landed,
// or a second reconcile planned before it, finds the removal done.
// [LAW:no-ambient-temporal-coupling] Every attempt is recorded
// (RemoteRemovals).
func (s *Store) SyncRemoveRemote(ctx context.Context, name string) error {
	trimmedName, err := requireSyncArg("remote", name)
	if err != nil {
		return err
	}
	owner := ownerOfHeld(trimmedName)
	return s.runSyncMutation(ctx, func(ctx context.Context) error {
		record := storage.RemoteRemovalRecord{Remote: trimmedName, Owner: owner.String()}
		err := s.removeRemote(ctx, owner, &record)
		s.removals = append(s.removals, record)
		return err
	})
}

func (s *Store) removeRemote(ctx context.Context, owner refOwner, record *storage.RemoteRemovalRecord) error {
	var err error
	if record.Held, err = s.holdsRemote(ctx, record.Remote); err != nil {
		return fmt.Errorf("remove dolt remote %q: %w", record.Remote, err)
	}
	if owner.String() != record.Remote {
		if record.OwnerHeld, err = s.holdsRemote(ctx, owner.String()); err != nil {
			return fmt.Errorf("remove dolt remote %q: %w", record.Remote, err)
		}
	}
	if !record.OwnerHeld {
		dropped, err := s.dropForkPoints(ctx, owner, nil)
		s.forkPoints = append(s.forkPoints, dropped...)
		if err != nil {
			return err
		}
		record.TrackingRefsDropped, err = s.dropTrackingRefs(ctx, owner)
		if err != nil {
			return err
		}
	}
	if record.Held {
		if _, err := callIntProcedure(ctx, s.db, "DOLT_REMOTE", "remove", record.Remote); err != nil {
			return fmt.Errorf("remove dolt remote %q: %w", record.Remote, err)
		}
	}
	return nil
}

// holdsRemote reads whether Dolt holds a remote named name.
func (s *Store) holdsRemote(ctx context.Context, name string) (bool, error) {
	var held int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_remotes WHERE name = ?`, name).Scan(&held); err != nil {
		return false, fmt.Errorf("read whether dolt holds remote %q: %w", name, err)
	}
	return held > 0, nil
}

// RemoteRemovals is every SyncRemoveRemote attempt since the store opened,
// oldest first.
func (s *Store) RemoteRemovals() []storage.RemoteRemovalRecord {
	return slices.Clone(s.removals)
}

// SyncSetRemoteURL re-points a remote in one mutation. Dolt has no set-url, so
// it is a remove and an add; the fork points stay, because the name still
// names the remote they record, and the next fetch brings its tracking refs
// back (SyncFetch drops fork points only after its fetch). A receive's landing
// brings back the sync branch's ref alone, so the sync branch's fork point,
// the one a reconcile reads, is the one it keeps (LandFetchedHead). The remove runs only
// while the remote is there, so an attempt retried after its remove landed
// goes on to the add.
func (s *Store) SyncSetRemoteURL(ctx context.Context, name storage.RemoteName, url string) error {
	trimmedURL, err := requireSyncArg("remote url", url)
	if err != nil {
		return err
	}
	return s.runSyncMutation(ctx, func(ctx context.Context) error {
		present, err := s.holdsRemote(ctx, name.String())
		if err != nil {
			return fmt.Errorf("re-point dolt remote %q: %w", name, err)
		}
		if present {
			if _, err := callIntProcedure(ctx, s.db, "DOLT_REMOTE", "remove", name.String()); err != nil {
				return fmt.Errorf("re-point dolt remote %q: remove: %w", name, err)
			}
		}
		if _, err := callIntProcedure(ctx, s.db, "DOLT_REMOTE", "add", name.String(), trimmedURL); err != nil {
			return fmt.Errorf("re-point dolt remote %q: add %s: %w", name, trimmedURL, err)
		}
		return nil
	})
}

// SyncFetch always prunes: lit reads no branch but the sync branch, so a
// tracking ref of a branch the remote deleted serves nothing and only keeps
// that branch's history reachable through every GC, past the history bound.
// [LAW:dataflow-not-control-flow] one fetch, the same every call.
func (s *Store) SyncFetch(ctx context.Context, remote storage.RemoteName) error {
	owner := ownerOf(remote)
	// What the fetch pruned is read against the tracking refs as the first
	// attempt found them, before any fetch ran: an attempt retried after a
	// fetch that landed finds the pruned refs already gone.
	// [LAW:no-ambient-temporal-coupling] the before is recorded once, never
	// re-listed after the move.
	var before map[string]string
	var pruned []storage.PrunedRef
	err := s.runSyncMutation(ctx, func(ctx context.Context) error {
		tracked, err := s.trackingRefs(ctx, owner)
		if err != nil {
			return err
		}
		if before == nil {
			before = tracked
		}
		// The fork points are recorded in the same mutation as the fetch and
		// ahead of it, because the fetch moves every tracking ref of the remote
		// however the remote's history moved (recordForkPoints). Every attempt
		// records, so a retried attempt reports what it found as well.
		forkPoints, err := s.recordForkPoints(ctx, owner, tracked)
		s.forkPoints = append(s.forkPoints, forkPoints...)
		if err != nil {
			return err
		}
		// [LAW:single-enforcer] Remote transport retry wraps the one network-touching
		// call, inside the mutation's own GC-contention retry: the two loops recover
		// unrelated failures under separate budgets (see remote_io.go).
		err = s.runRemoteIO(ctx, func(ctx context.Context) error {
			_, err := callIntProcedure(ctx, s.db, "DOLT_FETCH", "--prune", remote.String())
			return err
		})
		if err != nil {
			return fmt.Errorf("fetch remote %q: %w", remote, err)
		}
		after, err := s.trackingRefs(ctx, owner)
		if err != nil {
			return err
		}
		pruned = prunedRefs(owner, before, after)
		// A fork point lives exactly as long as its tracking ref, so every one
		// the fetch left without a ref goes. [LAW:one-source-of-truth]
		dropped, err := s.dropForkPoints(ctx, owner, after)
		s.forkPoints = append(s.forkPoints, dropped...)
		return err
	})
	s.pruned = append(s.pruned, pruned...)
	return err
}

// PrunedRefs is every branch a fetch since the store opened found gone from
// its remote, oldest first.
func (s *Store) PrunedRefs() []storage.PrunedRef {
	return slices.Clone(s.pruned)
}

// SyncPull brings the local branch up to date with the remote by fetching and
// then converging: a strictly-behind branch fast-forwards, and a DIVERGED branch
// is healed by the field-aware reconcile engine — the SAME engine the automatic
// receive uses, so pull and auto-sync share one merge policy. [LAW:single-enforcer]
//
// It deliberately does NOT drive Dolt's native DOLT_PULL. Native pull performs a
// three-way merge in the working set, which requires autocommit disabled to hold
// conflicts — under the driver's default autocommit=on it aborts a conflicting
// pull with "@autocommit must be disabled so that merge conflicts can be
// resolved", leaving the divergence unresolved. The reconcile engine merges
// three exports in Go and replays one linear commit, so it never holds a Dolt
// conflict and never needs autocommit off — the autocommit failure is
// unrepresentable on this path. [LAW:types-are-the-program]
//
// It composes the receive's steps (fetch, follow a re-root, then fast-forward
// on the post-fetch freshness) and the reconcile rather than reimplementing
// either; the divergence branch is the only one that reconciles, and every
// other freshness state carries straight through. [LAW:dataflow-not-control-flow]
//
// NOTE: the inline auto-sync path (cli.performSyncReceive → performInlineReconcile)
// replicates this same receive-then-reconcile-on-divergence GATE rather than calling
// SyncPull, because it must record a separate automation trace per step and run the
// diverged-only reconcile on the one RW engine embedded Dolt permits. The merge
// POLICY is not duplicated — both paths call this package's SyncReconcile — but the
// gating is, so a change to which freshness states reconcile here must be mirrored
// there. [LAW:single-enforcer] the enforced invariant (the merge) has one home; the
// gate is a deliberate two-altitude replica, cross-referenced so neither drifts unseen.
//
// The whole converge — the settle and the reconcile — runs under ONE commit
// lock: acquireCommitLock is context-reentrant, so their own acquisitions
// short-circuit and the two steps are atomic against every other writer. The
// fetch and the follow run before it, each under its own hold: the follow takes
// Dolt's LOCK, which comes before the commit lock, and the converge reads
// whatever they left. That
// closes the TOCTOU window in which a concurrent push, landing between an
// independently-locked receive and reconcile, would make the local strictly
// behind — the reconcile would then read NotDiverged and the pull would report
// the contradictory "up to date, N behind". Freshness is computed from local
// refs that cannot move under the held lock, so the diverged state the receive
// saw is the state the reconcile heals. [LAW:no-ambient-temporal-coupling] one
// owner of the converge's atomicity.
func (s *Store) SyncPull(ctx context.Context, remote storage.RemoteName, branch string) (storage.SyncPullResult, error) {
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return storage.SyncPullResult{}, err
	}
	branch = trimmedBranch
	if err := s.SyncFetch(ctx, remote); err != nil {
		return storage.SyncPullResult{}, err
	}
	if err := s.followRemoteReRoot(ctx, remote, branch, storage.SyncEntryPull); err != nil {
		return storage.SyncPullResult{}, err
	}
	var result storage.SyncPullResult
	err = s.withCommitLock(ctx, func(ctx context.Context) error {
		recv, err := s.settleReceived(ctx, remote, branch)
		if err != nil {
			return err
		}
		result.Ahead, result.Behind = recv.Ahead, recv.Behind
		result.OldestDivergedUnix = recv.OldestDivergedUnix
		result.Base = recv.Base
		result.RemoteSchemaAhead = recv.RemoteSchemaAhead
		switch recv.State {
		case storage.SyncReceiveUpToDate:
			result.State = storage.SyncPullUpToDate
		case storage.SyncReceiveFastForwarded:
			result.State = storage.SyncPullFastForwarded
		case storage.SyncReceiveAhead:
			result.State = storage.SyncPullAhead
		case storage.SyncReceiveNeverSynced:
			result.State = storage.SyncPullNeverSynced
		case storage.SyncReceiveDiverged:
			rec, err := s.reconcile(ctx, remote, branch, autonomousSettle, detectOnly)
			if err != nil {
				return err
			}
			result.Ahead, result.Behind = rec.Ahead, rec.Behind
			result.Landing = rec.Landing
			switch rec.State {
			case storage.SyncReconcileLinearized:
				result.State = storage.SyncPullLinearized
				// rec's ahead/behind describe the divergence that was just healed,
				// not the outcome. Re-read so the reported counts match the linear
				// result (the merge commit sits on the remote head: 1 ahead, 0
				// behind). Consistent under the single lock — freshness cannot move.
				// The divergence timestamp is re-read from the SAME freshness for the
				// same reason: leaving it at the pre-merge value would date a
				// divergence that no longer exists, a field contradicting its state.
				// [LAW:one-source-of-truth] the counts, the timestamp, and the state all agree.
				fresh, err := s.SyncFreshness(ctx, remote, branch)
				if err != nil {
					return err
				}
				result.Ahead, result.Behind = fresh.Ahead, fresh.Behind
				result.OldestDivergedUnix = fresh.OldestDivergedUnix
			case storage.SyncReconcileProsePending:
				result.State = storage.SyncPullProsePending
				result.Pending = rec.Pending
			case storage.SyncReconcileUnrelated:
				// No common ancestor: nothing merged, nothing committed. The divergence
				// is real and still present, so the ahead/behind counts and the fork
				// timestamp the receive recorded ride along unchanged — the fork the
				// receive saw is the fork this reports. [LAW:one-source-of-truth] The
				// both-sides inventory the reconcile read off the two anchors rides along
				// too, so the pull surface shows what each side holds.
				result.State = storage.SyncPullUnrelated
				result.Unrelated = rec.Unrelated
			case storage.SyncReconcileIDCollision:
				// An id names a different ticket on each side: the merge was refused and
				// nothing committed, so the divergence stands exactly as the receive
				// measured it — counts and fork timestamp ride along unchanged, like the
				// unrelated case above. The two rows travel whole; the pull surface is
				// the only place this machine can see the other side's ticket.
				result.State = storage.SyncPullIDCollision
				result.Collisions = rec.Collisions
			case storage.SyncReconcileNotDiverged:
				// Under the single lock a push race cannot resolve the divergence
				// between the receive and the reconcile, so this is the benign
				// idempotent case (the divergence was already gone). Nothing to merge.
				result.State = storage.SyncPullUpToDate
				// Up-to-date has no divergence, so the timestamp the receive recorded
				// must not ride along — it would date a fork that is gone, a field
				// contradicting its state. [LAW:one-source-of-truth]
				result.OldestDivergedUnix = 0
			default:
				// A reconcile state this mapping does not know would otherwise fall
				// through as the zero-value "" and be rendered downstream as a bland
				// "ok" — masking the gap. Fail at the source instead. [LAW:no-silent-failure]
				return fmt.Errorf("sync pull: unhandled reconcile state %q", rec.State)
			}
		default:
			// Same guard for the receive enum: an unmapped receive state must not
			// silently become an empty pull state. [LAW:no-silent-failure]
			return fmt.Errorf("sync pull: unhandled receive state %q", recv.State)
		}
		return nil
	})
	if err != nil {
		return storage.SyncPullResult{}, err
	}
	return result, nil
}

// LocalIssueCount reports how many issues the local data branch holds. It is the
// adopt-safety signal for `lit init`: a store with zero local issues has no work
// to lose, so adopting the remote history wholesale is safe; a store with issues
// must be preserved. A store that has never been opened for normal use has not
// run the baseline migration, so the issues table is simply absent — a true "no
// issues yet" state in the schema lifecycle, reported as 0 rather than surfaced
// as a missing-table error. [LAW:no-defensive-null-guards] The absence is a real
// domain value (pristine store), matched here, not papered over.
func (s *Store) LocalIssueCount(ctx context.Context) (int64, error) {
	var tableExists int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'issues'`,
	).Scan(&tableExists); err != nil {
		return 0, fmt.Errorf("check issues table presence: %w", err)
	}
	if tableExists == 0 {
		return 0, nil
	}
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count local issues: %w", err)
	}
	return count, nil
}

// SyncResetToRemoteHead replaces the local data branch with the remote-tracking
// ref's history, wholesale — the embedded equivalent of `git reset --hard
// remotes/<remote>/<branch>`. It is the bootstrap counterpart to SyncPull, not a
// variant of it: a freshly-initialized store's only commit is an unrelated
// bootstrap root, so a merge against the remote fails with "no common ancestor".
// Adopting the remote head discards that throwaway root and points the local
// branch at the remote history. It is therefore destructive of local commits by
// design — the one safe caller is `lit init` on a store it just created, where
// there is no local history to lose. The caller has already fetched, so the
// tracking ref exists; this method owns only the reset. [LAW:decomposition]
func (s *Store) SyncResetToRemoteHead(ctx context.Context, remote storage.RemoteName, branch string) error {
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return err
	}
	trackingRef := fmt.Sprintf("remotes/%s/%s", remote, trimmedBranch)
	return s.runSyncMutation(ctx, func(ctx context.Context) error {
		return resetHardToRef(ctx, s.db, trackingRef)
	})
}

// resetHardToRef hard-resets the active branch to a ref, adopting its history
// wholesale — the embedded equivalent of `git reset --hard <ref>`. It is the one
// reset primitive shared by SyncResetToRemoteHead (the `lit init` adopt), the
// unrelated-histories take-remote resolution, a checkpoint's reset, and a
// refused batch's return to the commit it started from, so none can drift on
// how a hard reset is spelled. [LAW:single-enforcer] The caller binds the ref
// to a real one — a tracking ref, a checkpoint branch, a commit; this owns only
// the reset.
func resetHardToRef(ctx context.Context, db *sql.DB, ref string) error {
	if _, err := callIntProcedure(ctx, db, "DOLT_RESET", "--hard", ref); err != nil {
		return fmt.Errorf("reset to %q: %w", ref, err)
	}
	return nil
}

// resetExactlyToRef leaves the working set exactly ref's. A hard reset drops
// the tables staged since ref but keeps the unstaged ones it lacks, such as a
// migration's Up creates, so DOLT_CLEAN drops those first. In that order a
// failure between the two leaves the reset's own state still to do, for the
// caller to run again. [LAW:single-enforcer] every reset that must not keep
// tables ref lacks.
func resetExactlyToRef(ctx context.Context, db *sql.DB, ref string) error {
	if err := execProcedureDiscard(ctx, db, "DOLT_CLEAN"); err != nil {
		return fmt.Errorf("clean before the reset to %q: %w", ref, err)
	}
	return resetHardToRef(ctx, db, ref)
}

// SyncReceive fetches the remote and, only when the local branch is strictly
// behind, lands it on the remote head. It is purely lossless: it never creates
// a merge commit, never merges a divergence, and never leaves a dirty or
// conflicted working set — the freshness that licenses the landing counts
// every local write, stranded ones included (freshnessOverEveryWrite), so the
// landing discards none. [LAW:effects-at-boundaries] The one state that moves
// the branch (behind → land on the remote head) is the only safe automatic one;
// every other state is observed and reported for the caller, with the diverged
// case explicitly deferred to the foreground agent-present reconcile rather than
// silently dropped. [LAW:dataflow-not-control-flow] The post-fetch freshness is
// the value that selects the outcome; there is one fetch and one freshness read
// every call. The fetch and the settle are separate holds, and between them the
// settle follows a re-root the fetch revealed.
func (s *Store) SyncReceive(ctx context.Context, remote storage.RemoteName, branch string) (storage.SyncReceiveResult, error) {
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return storage.SyncReceiveResult{}, err
	}

	if err := s.SyncFetch(ctx, remote); err != nil {
		return storage.SyncReceiveResult{}, err
	}
	return s.SyncSettleReceived(ctx, remote, trimmedBranch)
}

// SyncSettleReceived is SyncReceive without the fetch: it reads the tracking
// ref as it already stands — a fetch landed on this store from a clone
// (LandFetchedHead) — and lands on it when the local branch is strictly
// behind it, first following a re-root the tracking ref reveals
// (followRemoteReRoot). It contacts no network.
func (s *Store) SyncSettleReceived(ctx context.Context, remote storage.RemoteName, branch string) (storage.SyncReceiveResult, error) {
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return storage.SyncReceiveResult{}, err
	}
	if err := s.followRemoteReRoot(ctx, remote, trimmedBranch, storage.SyncEntryReceive); err != nil {
		return storage.SyncReceiveResult{}, err
	}
	return s.settleReceived(ctx, remote, trimmedBranch)
}

// settleReceived runs settleReceivedWithinLock as a sync mutation, joining a
// commit lock the caller already holds, and records that the settle succeeded
// (settled.last, receive_episode.go). Every settle runs through here, so this
// is the one writer of that record whatever surface settled.
// [LAW:single-enforcer] The record is bookkeeping after the settle completed,
// not part of it: a record that could not be written leaves the settle's
// result standing, so the caller's reconcile and push go on, and the failure
// is printed and lands on the event (SettledLastWrites).
// [LAW:no-silent-failure] [LAW:nothing-unseen]
func (s *Store) settleReceived(ctx context.Context, remote storage.RemoteName, branch string) (storage.SyncReceiveResult, error) {
	var result storage.SyncReceiveResult
	err := s.runSyncMutation(ctx, func(ctx context.Context) error {
		var err error
		result, err = s.settleReceivedWithinLock(ctx, remote, branch)
		return err
	})
	if err != nil {
		return storage.SyncReceiveResult{}, err
	}
	var write storage.SettledLastWrite
	if err := writeSettledLast(s.doltRootDir, time.Now()); err != nil {
		write.Err = err.Error()
		fmt.Fprintf(s.stderr, "lit: the settle landed and stands, but this checkout's record of it (settled.last) was not written: %v\n", err)
	}
	s.settledLastWrites = append(s.settledLastWrites, write)
	return result, nil
}

// strandedWritesCommitMessage is the message of the commit a sync lands over
// writes a failed DOLT_COMMIT left in the working set.
const strandedWritesCommitMessage = "commit writes a failed commit left staged"

// freshnessOverEveryWrite is the freshness that decides whether the branch
// moves. Freshness counts commits, and landing on the remote head or on a
// reconcile's replay resets the working set, so stranded writes are versioned
// first (commitStrandedWrites). Then the freshness
// counts every local write: a stranded write is a local commit, and the branch
// is diverged, not behind. [LAW:single-enforcer] The settle and the reconcile
// plan both read it.
func (s *Store) freshnessOverEveryWrite(ctx context.Context, remote storage.RemoteName, branch string) (storage.SyncFreshness, error) {
	if err := s.commitStrandedWrites(ctx, strandedWritesCommitMessage); err != nil {
		return storage.SyncFreshness{}, err
	}
	return s.SyncFreshness(ctx, remote, branch)
}

// commitStrandedWrites versions writes that stand in the working set with no
// commit over them, and records the commit it landed. A mutation commits inside
// its transaction and leaves none, so they come from a store an older lit wrote,
// or from a migration or a reconcile's schema lift whose DOLT_COMMIT failed.
// Anything about to hard-reset the branch runs it first, so the reset never
// discards a write that is not its own. [LAW:single-enforcer] the one place a
// stranded write is committed. [LAW:no-silent-failure] [LAW:nothing-unseen]
// The caller holds the commit lock, so a step that reads interrupted is a dead
// process's, and the commit would version it: one this binary may write over
// is undone first, and one it may not is refused.
func (s *Store) commitStrandedWrites(ctx context.Context, message string) error {
	if err := s.undoDeadStep(ctx); err != nil {
		return err
	}
	tables, err := s.strandedTables(ctx)
	if err != nil {
		return err
	}
	return s.landStrandedWrites(ctx, tables, message)
}

// undoDeadStep undoes the interrupted step the working set holds, if any.
// Operability is read only for one, so a store with none commits as before.
func (s *Store) undoDeadStep(ctx context.Context) error {
	state, err := s.classifyMigrationState(ctx)
	if err != nil || !state.interrupted() {
		return err
	}
	state, op, err := s.assessOperability(ctx)
	if err != nil {
		return err
	}
	if !state.undoDue(op) {
		return fmt.Errorf("the step from v%d stands uncommitted, and this lit may not write over it", state.committedVersion)
	}
	return s.undoInterruptedLocked(ctx, state)
}

// landStrandedWrites commits the working set over the tables a look at it
// counted dirty, and records the look.
func (s *Store) landStrandedWrites(ctx context.Context, tables int, message string) error {
	if err := s.commitWorkingSetOnce(ctx, s.db, commitStamp{Message: message}); err != nil {
		return err
	}
	if tables > 0 {
		s.strandedWritesCommits = append(s.strandedWritesCommits, storage.StrandedWritesCommit{Tables: tables})
	}
	s.strandedWritesLooked = true
	return nil
}

// strandedTables is how many tables stand dirty in the working set.
func (s *Store) strandedTables(ctx context.Context) (int, error) {
	var tables int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT table_name) FROM dolt_status`).Scan(&tables); err != nil {
		return 0, fmt.Errorf("read the working set: %w", err)
	}
	return tables, nil
}

// settleReceivedWithinLock is the half of a receive that follows the fetch:
// the post-fetch freshness selects the outcome, and the one state that moves
// the branch (behind) lands on the remote head. SyncReceive and
// SyncSettleReceived both run it, so a receive whose fetch ran here and one
// whose fetch ran on a clone settle identically. [LAW:single-enforcer] The
// caller holds the commit lock.
//
// The remote head's schema is read in every state and reported, so the
// settle carries what it learned about the remote rather than leaving the
// surface to infer it from the state: an up-to-date or ahead settle proves
// nothing about the remote's schema by its state alone. The behind landing
// adopts the head at that schema whatever it is; the landing writes no
// commit, so the reconcile's refusal has nothing to guard here, and the open
// is the one place that decides what this binary may do with the store it
// finds. [LAW:dataflow-not-control-flow] [LAW:nothing-unseen]
func (s *Store) settleReceivedWithinLock(ctx context.Context, remote storage.RemoteName, branch string) (storage.SyncReceiveResult, error) {
	var result storage.SyncReceiveResult
	fresh, err := s.freshnessOverEveryWrite(ctx, remote, branch)
	if err != nil {
		return result, err
	}
	result.Ahead, result.Behind = fresh.Ahead, fresh.Behind
	result.OldestDivergedUnix = fresh.OldestDivergedUnix
	result.Base = fresh.Base.Source
	remoteHead, err := s.freshnessHeadSchema(ctx, fresh, remote, branch)
	if err != nil {
		return result, err
	}
	if result.RemoteSchemaAhead, err = judgeSchemaAhead(remoteHead); err != nil {
		return result, err
	}
	switch fresh.State() {
	case storage.SyncBehind:
		// A reset, not a --ff-only merge, because behind includes a fork-point
		// base whose heads share no commit; along shared history it is the
		// fast-forward. The freshness that chose behind was read under this
		// commit lock, so no local commit is past the remote.
		if err := resetHardToRef(ctx, s.db, fmt.Sprintf("remotes/%s/%s", remote, branch)); err != nil {
			return result, err
		}
		result.State = storage.SyncReceiveFastForwarded
	case storage.SyncDiverged:
		result.State = storage.SyncReceiveDiverged
	case storage.SyncAhead:
		result.State = storage.SyncReceiveAhead
	case storage.SyncNeverSynced:
		result.State = storage.SyncReceiveNeverSynced
	case storage.SyncUpToDate:
		result.State = storage.SyncReceiveUpToDate
	default:
		// A freshness state this mapping does not know must not settle as
		// up to date, where neither the receive nor the reconcile gate would
		// act on it. [LAW:no-silent-failure]
		return result, fmt.Errorf("sync receive: unhandled freshness state %q", fresh.State())
	}
	return result, nil
}

// compactWithinLock runs DOLT_GC at the requested depth, rotates the
// connection, and then sweeps the remote cache's orphaned instance refs. The
// caller must already hold the commit lock; SyncCompact and
// SyncCompactAndPush both compose over this helper so the compact step has one
// implementation regardless of whether it runs as a standalone mutation or as
// the first step of a larger one.
//
// The depth arrives as an argument rather than being decided here, so this
// function has no policy in it and the two callers cannot drift on what a
// given depth means. [LAW:dataflow-not-control-flow]
// It returns what the attempt knows about itself alongside any error, because
// only this function can observe whether DOLT_GC landed — everything above it
// sees one error and cannot tell a pass that never ran from a pass that ran and
// was followed by a failure. [LAW:types-are-the-program]
//
// The sweep is here, after the rotation, because this is the one moment
// orphaned instance refs can be told from live ones: this process holds no
// remote database open (rotateAfterGC), nothing has opened one since, and
// Dolt's LOCK and the commit lock keep every other writer of the mirrors —
// another write engine, a receive's landing — off the store. So every path that
// compacts collects the refs, and none can sweep without that moment.
// [LAW:no-ambient-temporal-coupling] A sweep that fails reports in its outcome
// rather than failing the compaction that already rewrote the store.
func (s *Store) compactWithinLock(ctx context.Context, mode GCMode) (storage.CompactionPass, refSweepOutcome, error) {
	attempt := storage.CompactionPass{Depth: mode}
	defer func() { s.compactionPasses = append(s.compactionPasses, attempt) }()
	args, err := gcProcedureArgs(mode)
	if err != nil {
		return attempt, refSweepOutcome{}, err
	}
	// Exec, because a row read after the collection is a second chance for a
	// deadline to read a finished pass as failed: database/sql closes a Rows on
	// ctx in a goroutine racing the read. DOLT_GC's status says nothing its error
	// does not.
	if _, err := s.db.ExecContext(ctx, buildProcedureCall("DOLT_GC", len(args)), stringArgsToAny(args)...); err != nil {
		return attempt, refSweepOutcome{}, fmt.Errorf("compact dolt store (%s): %w", mode, err)
	}
	// Recorded here, before the rotation below and not after it: DOLT_GC has
	// returned, so the store on disk is already rewritten and nothing that
	// follows can un-rewrite it. A rotation failure must not read as a pass
	// that never happened. [LAW:no-silent-failure]
	attempt.Ran = true
	// [LAW:single-enforcer] Online GC poisons the active SQL connection; the Store rotates it here so every downstream query contract is restored before lock release.
	if attempt.Rotation, err = s.rotateAfterGC(ctx); err != nil {
		return attempt, refSweepOutcome{}, err
	}
	return attempt, s.sweepOrphanedInstanceRefs(ctx), nil
}

// rotateAfterGC gets a pass a usable connection back, which also owes the sweep
// after it a second thing: no remote database open in this process. It returns
// how it rotated, and nothing when the rotation failed, so the event never says
// a store recovered that did not.
//
// Replacing the connection answers the first and keeps Dolt's LOCK, so no
// writer can take it in a gap and stand the queue behind it still
// (replaceConnection). It answers the second only on an engine that has reached
// no remote: a pushed remote stays open until its engine closes, and a sweep
// beside it removes the push's live refs. Only Store.reconnect closes that. So
// the Store's own fact picks the rotation, and no caller does. A fetch closes
// its remote and counts anyway; that costs one rotation of the engine.
// [LAW:no-ambient-temporal-coupling]
//
// This restores the store rather than doing the caller's work, so a deadline or
// a cancel that lands after DOLT_GC returned does not cut it, as it does not cut
// withEngineClosed's reopen; the engine open's own wait on Dolt's LOCK bounds it.
func (s *Store) rotateAfterGC(ctx context.Context) (storage.CompactionRotation, error) {
	ctx = context.WithoutCancel(ctx)
	rotation, rotate := storage.CompactionRotationConnection, s.replaceConnection
	if s.reachedRemote {
		rotation, rotate = storage.CompactionRotationEngine, s.reconnect
	}
	if err := rotate(ctx); err != nil {
		return "", err
	}
	return rotation, nil
}

// CompactionPasses is every compaction attempted on this store since it opened,
// oldest first, and whether anything looked to see if one was owed.
func (s *Store) CompactionPasses() ([]storage.CompactionPass, bool) {
	return slices.Clone(s.compactionPasses), s.compactionLooked || len(s.compactionPasses) > 0
}

// SyncCompact compacts the store at the requested depth, with no remote
// involved. It is what a workspace that never pushes has to reach for, and the
// depth is the caller's to choose.
//
// The before/after compaction signals are measured around the pass and returned already
// rendered, so a caller learns what was reclaimed without having to read this
// engine's on-disk layout for itself. The two readings straddle the lock rather
// than sitting inside it: they are an account of what happened, never an input
// to a decision, so a concurrent mutation nudging them costs accuracy in a
// report and nothing else.
func (s *Store) SyncCompact(ctx context.Context, mode GCMode) (storage.CompactionOutcome, error) {
	if !mode.Valid() {
		// [LAW:no-silent-failure] an illegal depth must never reach the pass and
		// silently collapse to the shallower default; reject it at the door, with
		// the legal values named. The contract owns which those are, so every
		// engine refuses the same set. [LAW:single-enforcer]
		return storage.CompactionOutcome{}, fmt.Errorf("compact: illegal depth %d (want %q or %q)", int(mode), storage.GCNewGen, storage.GCFull)
	}
	before, beforeErr := s.measureCompactionSignals()
	var attempt storage.CompactionPass
	var sweep refSweepOutcome
	// [LAW:single-enforcer] Dolt garbage collection is exposed through a single Store entrypoint so every caller routes through the same commit-lock and retry wrapper.
	err := s.runSyncMutation(ctx, func(ctx context.Context) error {
		pass, swept, compactErr := s.compactWithinLock(ctx, mode)
		attempt = attempt.Plus(pass)
		sweep = sweep.plus(swept)
		return compactErr
	})
	// Measured on both paths, because a pass that ran and then failed to
	// rotate rewrote the store exactly as much as one that succeeded. The
	// reading survives a dead connection: measureCompactionSignals is one Stat and one
	// ReadDir against the filesystem, with no engine and no lock involved.
	after, afterErr := s.measureCompactionSignals()
	// The sweep's line joins the signals': both are this pass's account of
	// what it changed on disk, and Detail is where that account goes.
	outcome := attempt.Outcome(joinMaintenance(signalsDelta(before, after, errors.Join(beforeErr, afterErr)), sweep.Report()))
	if err != nil {
		// The outcome is returned populated ALONGSIDE the error, because it
		// describes the compaction and the compaction may well have happened —
		// compactWithinLock ends on a rotation, and a failure there
		// leaves a store that really was collected. Reporting Ran: false would
		// be this value lying about its own subject, and it is the one thing a
		// caller must not conclude from a maintenance failure.
		// [LAW:no-silent-failure]
		//
		// This is not the arrangement SyncCompactAndPush declines below. That
		// method returns a SyncPushResult describing a PUSH that genuinely did
		// not happen, so populating it would lie; this one describes a
		// compaction that genuinely may have. A result must tell the truth about
		// its own subject — which is a different rule from "never return a value
		// beside an error", and the reason the two methods differ.
		//
		// The error carries the same account for whoever is reading stderr
		// rather than the trail, through the filter compactionReport already
		// owns: a routine shallow pass stays silent, a deep one speaks.
		return outcome, annotateWithMaintenance(err, joinMaintenance(compactionReport(attempt, nil), sweep.Report()))
	}
	return outcome, nil
}

// MaintainIfDue is the backstop's entrypoint: it compacts when this store's
// compaction signals say a pass is owed, then holds the store under its size
// ceiling by retiring automatic recovery snapshots, and reports both with the
// footprint the probe left. The judgments live here, beside the layout they
// read, so a cadence owner never has to know what a journal or a snapshot
// is. [LAW:decomposition]
//
// Both steps run every time, and the measurements decide how much each does:
// "nothing was due" is a zero compaction, "within the ceiling" is nothing
// pruned. Neither step is skipped for the other's failure — a compaction
// that failed leaves the snapshots exactly as prunable — and their errors
// are joined. [LAW:dataflow-not-control-flow] [LAW:no-silent-failure]
//
// The ceiling is never an input to compaction: a store over its ceiling with
// nothing to reclaim runs no pass it did not already owe.
func (s *Store) MaintainIfDue(ctx context.Context) (storage.MaintenanceOutcome, error) {
	compaction, compactErr := s.compactIfDue(ctx)
	held, pruneErr := s.pruneAutomaticSnapshots(ctx)
	return storage.MaintenanceOutcome{
		Compaction:      compaction,
		SnapshotsPruned: held.pruned,
		Footprint:       held.footprint,
	}, errors.Join(compactErr, pruneErr)
}

// compactIfDue compacts only when this store's compaction signals say a pass
// is owed. "Nothing was due" returns a zero outcome and no error, because it
// is the ordinary result — the check is cheap and meant to be asked often,
// while the pass is neither.
func (s *Store) compactIfDue(ctx context.Context) (storage.CompactionOutcome, error) {
	signals, err := s.measureCompactionSignals()
	if err != nil {
		// "I could not measure" is not "nothing is due". Returning the error
		// keeps an unreadable store from quietly ceasing to be maintained.
		// [LAW:no-silent-failure]
		return storage.CompactionOutcome{}, fmt.Errorf("measure compaction signals: %w", err)
	}
	mode, due := dueMode(signals)
	if !due {
		s.compactionLooked = true
		return storage.CompactionOutcome{}, nil
	}
	return s.SyncCompact(ctx, mode)
}

// chooseCompactionDepth picks the depth for the push path. The push always
// compacts at least the new generation — that is this path's contract and it
// does not become conditional on a measurement succeeding — so the signals
// can only ever deepen the pass, never cancel it.
//
// A measurement that fails therefore returns a usable depth AND its error: the
// fallback is this path's contract, the new generation alone, which makes it a
// safe floor rather than an invented one, and the caller reports the problem
// instead of the store silently choosing for it.
// [LAW:no-silent-failure]
func (s *Store) chooseCompactionDepth() (GCMode, error) {
	signals, err := s.measureCompactionSignals()
	if err != nil {
		return GCNewGen, err
	}
	if mode, due := dueMode(signals); due {
		return mode, nil
	}
	return GCNewGen, nil
}

// joinFollowFailure is a failed push as its caller reports it: the push's
// error, with the fetch or follow that could not explain a refusal joined, or
// nil when the push landed. The join carries that check's text and not its
// error: the push's failure is what a caller classifies, and a check that
// met a busy store or a cancellation after the remote refused does not make
// the refusal busy or canceled. Every check this package runs after a
// refusal joins its failure the same way.
func joinFollowFailure(pushErr, followErr error) error {
	if followErr != nil {
		return fmt.Errorf("%w (and fetching and following the remote, which could explain the refusal, failed: %v)", pushErr, followErr)
	}
	return pushErr
}

// pushRefusedError is a push the remote refused on its history: it holds
// commits the pushed head does not descend from, or shares no ancestor with
// it. Only a refusal can be a re-root of the remote's history, so it is the
// one push failure retryPushAfterFollow acts on. [LAW:parse-dont-validate]
// the failure is classified once, where DOLT_PUSH returns it.
type pushRefusedError struct {
	remote string
	cause  error
}

func (e pushRefusedError) Error() string {
	return fmt.Sprintf("push remote %q: %v", e.remote, e.cause)
}

func (e pushRefusedError) Unwrap() error { return e.cause }

// pushRefusals are the errors DOLT_PUSH wraps when the remote refused the
// push on its history:
//   - doltdb.ErrIsAhead, actions.ErrCantFF and datas.ErrMergeNeeded are the
//     target rejections DoPush reports as non-fast-forward: the remote holds
//     commits the pushed head does not descend from.
//   - doltdb.ErrNoCommonAncestor is a re-rooted remote, which shares no
//     ancestor with the push.
//
// All come from the fast-forward check, which a forced push skips, so a
// forced push is never refused on its history: its lease is what refuses it
// (pushForcedWithinLock), and a lost lease is storage.ForcedPushLeaseError,
// never one of these.
var pushRefusals = []error{doltdb.ErrIsAhead, actions.ErrCantFF, datas.ErrMergeNeeded, doltdb.ErrNoCommonAncestor}

// classifyPushFailure is DOLT_PUSH's error as pushRefusedError when the remote
// refused it, and otherwise the failure as it came: an unreachable remote, one
// that never opened, a write the remote's storage failed, or the caller's
// context ending. An unreachable remote is already a parsed type, so its
// rendering is never read [LAW:parse-dont-validate].
func classifyPushFailure(remote storage.RemoteName, pushErr error) error {
	wrapped := fmt.Errorf("push remote %q: %w", remote, pushErr)
	if errors.As(pushErr, new(RemoteUnreachableError)) {
		return wrapped
	}
	if slices.ContainsFunc(pushRefusals, func(refusal error) bool { return errors.Is(pushErr, refusal) }) {
		return pushRefusedError{remote: remote.String(), cause: pushErr}
	}
	return wrapped
}

// pushMutation is one push as its own sync mutation.
func (s *Store) pushMutation(ctx context.Context, remote storage.RemoteName, branch string, setUpstream bool, force bool) (storage.SyncPushResult, error) {
	var result storage.SyncPushResult
	err := s.runSyncMutation(ctx, func(ctx context.Context) error {
		pushed, pushErr := s.pushWithinLock(ctx, remote, branch, setUpstream, force)
		result = pushed
		return pushErr
	})
	if err != nil {
		return storage.SyncPushResult{}, err
	}
	return result, nil
}

// retryPushAfterFollow takes the outcome of a push the caller ran. A push the
// remote refused (pushRefusedError) is followed by a fetch and a follow of a
// re-root the fetch revealed (moveOntoRemoteReRoot). When the branch crossed
// the cut, by this follow or by another writer's in the gap before its hold,
// the refusal was the re-root's and the crossing removed it, so the
// push runs once more, as a plain push, and that push's outcome is the
// result. Otherwise the refusal stands, returned as pushErr with the fetch
// already run, so a caller judging it reads the remote as it stands now. A
// refusal that stands, the first push's or the retry's, comes back judged by
// judgeStandingRefusal: every push step's refusal passes through here, so one
// judgment covers them all. [LAW:single-enforcer]
// followErr is a fetch or follow that failed, beside the refusal it could not
// explain, for the caller to word. The follow collects nothing, so no GC
// stands between it and the retry it exists for; a store that outlives the
// sync collects after it (collectOwedCut). Every other outcome — a push that
// landed, or one that failed without a refusal (an unreachable remote, a
// commit lock not acquired, a compaction that failed first) — passes through
// untouched with nothing fetched.
func (s *Store) retryPushAfterFollow(ctx context.Context, remote storage.RemoteName, branch string, setUpstream bool, pushed storage.SyncPushResult, pushErr error) (result storage.SyncPushResult, _ error, followErr error) {
	trimmedBranch := strings.TrimSpace(branch)
	var refusal pushRefusedError
	if !errors.As(pushErr, &refusal) {
		return pushed, pushErr, nil
	}
	if err := s.SyncFetch(ctx, remote); err != nil {
		return storage.SyncPushResult{}, pushErr, err
	}
	_, crossed, err := s.moveOntoRemoteReRoot(ctx, remote, trimmedBranch, storage.SyncEntryPush)
	if err != nil {
		return storage.SyncPushResult{}, pushErr, err
	}
	result, pushErr = s.retryOverFollow(ctx, remote, trimmedBranch, setUpstream, crossed, pushErr)
	return result, pushErr, nil
}

// retryOverFollow is retryPushAfterFollow once the follow finished: the one
// retry when the branch crossed the cut, then the judgment of a refusal that
// still stands.
func (s *Store) retryOverFollow(ctx context.Context, remote storage.RemoteName, branch string, setUpstream bool, crossed bool, pushErr error) (storage.SyncPushResult, error) {
	var refusal pushRefusedError
	if crossed {
		result, retryErr := s.pushMutation(ctx, remote, branch, setUpstream, false)
		if !errors.As(retryErr, &refusal) {
			return result, retryErr
		}
		pushErr = retryErr
		// The retry was refused over a remote that moved after the first
		// fetch, so its judgment reads the remote as it stands now too.
		if err := s.SyncFetch(ctx, remote); err != nil {
			return storage.SyncPushResult{}, unjudgedRefusal(pushErr, err)
		}
	}
	return storage.SyncPushResult{}, s.judgeStandingRefusal(ctx, remote, branch, pushErr)
}

// judgeStandingRefusal is a refusal that stands, after its fetch, as a
// storage.PushDivergedError when the branch and the remote each hold commits
// the other lacks.
func (s *Store) judgeStandingRefusal(ctx context.Context, remote storage.RemoteName, branch string, refusal error) error {
	fresh, err := s.SyncFreshness(ctx, remote, branch)
	if err != nil {
		return unjudgedRefusal(refusal, err)
	}
	if fresh.CommitsDiverged() {
		return storage.PushDivergedError{Remote: remote, Branch: branch, Refusal: refusal}
	}
	return refusal
}

// unjudgedRefusal is a refusal whose divergence could not be read, still a
// pushRefusedError for every caller that judges it further.
func unjudgedRefusal(refusal, readErr error) error {
	return fmt.Errorf("%w (and whether the branch diverged from the remote could not be read: %v)", refusal, readErr)
}

// SyncCompactAndPush compacts then pushes under one commit-lock acquisition, so
// no other mutation interleaves between the garbage collection and the push and
// the push reflects exactly the compacted state. [LAW:no-ambient-temporal-coupling]
// The explicit `lit sync push` and the pre-push hook use this; the on-change
// mirror uses SyncPushFromClone. They are distinct single-purpose entrypoints,
// not one method with a compaction flag. [LAW:decomposition] Both end in
// keepHistoryBoundAfter, so whichever landed the push keeps the bound. This
// one runs on the live store, so it then collects below the cut once,
// whether its retried push or its bound followed (collectOwedCut), and the
// collection joins the maintenance account, or the error's when the push
// failed: a pass that ran is reported either way. [LAW:no-silent-failure]
func (s *Store) SyncCompactAndPush(ctx, boundLifetime context.Context, remote storage.RemoteName, branch string, window storage.BoundWindow, setUpstream bool, force bool) (storage.SyncPushResult, error) {
	pushed, err := s.compactAndPush(ctx, remote, branch, setUpstream, force)
	pushed, err = s.keepHistoryBoundAfter(boundLifetime, remote, branch, window, pushed, err)
	collected := collectionReport(s.collectOwedCut(ctx, storage.SyncEntryPush))
	if err != nil {
		return storage.SyncPushResult{}, annotateWithMaintenance(err, collected)
	}
	pushed.Maintenance = joinMaintenance(pushed.Maintenance, collected)
	return pushed, nil
}

// compactAndPush is SyncCompactAndPush up to the push landing.
func (s *Store) compactAndPush(ctx context.Context, remote storage.RemoteName, branch string, setUpstream bool, force bool) (storage.SyncPushResult, error) {
	// The branch is refused before the compaction, which rewrites the store
	// under the commit lock, not after it in the push.
	branch, err := requireSyncArg("branch", branch)
	if err != nil {
		return storage.SyncPushResult{}, err
	}
	var attempt storage.CompactionPass
	var depthErr error
	var sweep refSweepOutcome
	var result storage.SyncPushResult
	err = s.runSyncMutation(ctx, func(ctx context.Context) error {
		// Measured inside the lock, where no other mutation can grow the
		// journal between the reading and the pass it selects.
		var depth GCMode
		depth, depthErr = s.chooseCompactionDepth()
		pass, swept, compactErr := s.compactWithinLock(ctx, depth)
		attempt = attempt.Plus(pass)
		sweep = sweep.plus(swept)
		if compactErr != nil {
			return compactErr
		}
		var pushErr error
		result, pushErr = s.pushWithinLock(ctx, remote, branch, setUpstream, force)
		return pushErr
	})
	// A push refused over a re-root it follows runs again without
	// compacting.
	result, err, followErr := s.retryPushAfterFollow(ctx, remote, branch, setUpstream, result, err)
	err = joinFollowFailure(err, followErr)
	// DOLT_GC above compacts `noms`; the prune collects the other half of the
	// store's local footprint, the abandoned git remote mirrors. All three jobs
	// report whether or not the push landed. A pass that ran is reported even
	// though the call failed: a push failing afterwards does not un-rewrite the
	// store, and reporting only on the success path would lose a deep
	// collection entirely whenever the push it preceded failed — which is
	// exactly what explains an attempt that took minutes before failing. The
	// failure is where an operator looks when a command fails, so that is where
	// the account goes. [LAW:no-silent-failure] Whether a pass ran is the
	// attempt's to answer: the error may come from the compaction or from the
	// push after it, and compactionReport holds that filter.
	// [LAW:one-source-of-truth]
	//
	// The prune runs OUTSIDE the commit lock, because the lock was never what
	// made the removal safe: a directory is eligible only when NO configured
	// remote's key names it, and every open reaches a cache through a
	// configured remote's key, so an abandoned mirror is unreachable by
	// construction. Holding the cross-process writer-exclusion lock across an
	// unbounded filesystem walk would block every other lit mutation on this
	// workspace — `lit new`, `lit done`, all of them — to buy nothing. Nothing
	// bounds how many mirrors a long-lived workspace accumulates.
	//
	// Every outcome is carried, never raised: a cache that failed to collect
	// costs disk, while failing `lit sync push` over it would cost the user
	// their sync. [LAW:dataflow-not-control-flow] the jobs run every time and
	// report as values; nothing branches on whether they did. The maintenance
	// jobs share one operator-facing channel, each reporting only what is
	// worth saying. [LAW:one-type-per-behavior]
	maintenance := joinMaintenance(
		compactionReport(attempt, depthErr),
		sweep.Report(),
		s.pruneRemoteCache(ctx).Report(),
	)
	if err != nil {
		return storage.SyncPushResult{}, annotateWithMaintenance(err, maintenance)
	}
	result.Maintenance = maintenance
	return result, nil
}

// pushWithinLock runs DOLT_PUSH for the resolved remote and branch. The caller
// holds the commit lock (via runSyncMutation); every push composes over this
// one push implementation so the push step cannot drift
// between them. [LAW:single-enforcer]
func (s *Store) pushWithinLock(ctx context.Context, remote storage.RemoteName, branch string, setUpstream bool, force bool) (storage.SyncPushResult, error) {
	// Every push names its branch: the schema guard and a refusal's follow
	// both read that branch's tracking ref, so a push without one would skip
	// both. [LAW:types-are-the-program]
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return storage.SyncPushResult{}, err
	}
	if force {
		return s.pushForcedWithinLock(ctx, remote, trimmedBranch, setUpstream)
	}
	// R is the tracking ref's head as of the last fetch, read once for every
	// verdict the push runs (refuseSchemaSend); Dolt's fast-forward check then
	// refuses the push if the remote moved past it. A remote whose head is at
	// a schema above this binary's is no refusal here: a head at or below R
	// regresses nothing, and the open decided what this binary may write
	// (design-docs/schema-reversibility.md, "A push at the remote's own
	// schema lands"). [LAW:single-enforcer]
	remoteHead, err := s.trackingHeadSchema(ctx, remote, trimmedBranch)
	if err != nil {
		return storage.SyncPushResult{}, err
	}
	// HEAD is read BEFORE the push: the caller holds the commit lock, so HEAD
	// cannot move between this read and the push, and a read that fails
	// fails an attempt that has sent nothing. Read afterwards, a failure
	// (the push landing just inside its deadline, the read just outside)
	// would report a landed push as failed. [LAW:no-ambient-temporal-coupling]
	head, err := s.headCommitWithinLock(ctx)
	if err != nil {
		return storage.SyncPushResult{}, err
	}
	if err := s.refuseSchemaSend(ctx, remote, trimmedBranch, remoteHead, head); err != nil {
		return storage.SyncPushResult{}, err
	}
	args := []string{}
	if setUpstream {
		args = append(args, "--set-upstream")
	}
	args = append(args, remote.String(), fmt.Sprintf("HEAD:%s", trimmedBranch))
	query := buildProcedureCall("DOLT_PUSH", len(args))
	var result storage.SyncPushResult
	var message sql.NullString
	// One dropped SSH connection must not fail the whole push: the transport
	// retry absorbs it, and only a failure that survives the budget reaches the
	// caller — as RemoteUnreachableError naming the transport symptom, never as
	// the backend's misrendered authentication failure.
	pushErr := s.runRemoteIO(ctx, func(ctx context.Context) error {
		return s.db.QueryRowContext(ctx, query, stringArgsToAny(args)...).Scan(&result.Status, &message)
	})
	if pushErr != nil {
		return storage.SyncPushResult{}, classifyPushFailure(remote, pushErr)
	}
	result.Message = nullStringValue(message)
	result.Head = head
	return result, nil
}

// pushForcedWithinLock is a forced push: it replaces only the head it judged.
// R is the tracking ref's head as of the last fetch, and the push is the
// leased push on that head, so the remote takes it only while its branch
// still names R. A remote that moved since the last fetch keeps its head and
// the push is refused as storage.ForcedPushLeaseError, as is a forced push to
// a branch this checkout holds no head of: with no R there is nothing a force
// may replace, and a plain push creates a branch the remote lacks. A bare
// `--force` would skip Dolt's fast-forward check and overwrite a head the
// verdicts never read: an unheld lowering landed, a held head re-raised.
// [LAW:single-enforcer] the one leased push, with its one stale classifier,
// serves the history bound's re-root and `lit sync push --force` alike.
func (s *Store) pushForcedWithinLock(ctx context.Context, remote storage.RemoteName, branch string, setUpstream bool) (storage.SyncPushResult, error) {
	expected, synced, err := s.trackingHeadHash(ctx, remote, branch)
	if err != nil {
		return storage.SyncPushResult{}, err
	}
	if !synced {
		return storage.SyncPushResult{}, storage.ForcedPushLeaseError{Remote: remote, Branch: branch}
	}
	outcome, result, err := s.pushLeasedWithinLock(ctx, remote, "HEAD", branch, expected, setUpstream)
	if err != nil {
		return storage.SyncPushResult{}, err
	}
	if outcome == storage.LeaseStale {
		return storage.SyncPushResult{}, storage.ForcedPushLeaseError{Remote: remote, Branch: branch, Expected: expected}
	}
	return result, nil
}

// refuseSchemaSend is the three verdicts on the schema of the head a push
// sends, H, against the remote head it read, R: a raise above the ceiling
// never leaves this store (refuseSchemaAboveCeiling), where the ceiling is
// the lower of this lit's released-schema ceiling and the hold R carries; a
// schema above R is a raise only a release makes (refuseSchemaRaise); and a
// schema below R is a lowering only a held head makes
// (refuseSchemaLowering). A head at R passes all three: it is judged against
// R alone, whatever this binary's own schema or ceiling. One read of H and of
// the hold H carries serves all three. Every DOLT_PUSH runs it: the plain
// push here and the leased push in push_lease.go. [LAW:single-enforcer]
// [LAW:one-source-of-truth]
func (s *Store) refuseSchemaSend(ctx context.Context, remote storage.RemoteName, branch string, remoteHead remoteSchema, commitHash string) error {
	commitSchema, err := s.remoteHeadSchema(ctx, commitHash)
	if err != nil {
		return err
	}
	commitHold, err := s.schemaHoldAt(ctx, commitHash, "the head this push sends")
	if err != nil {
		return err
	}
	ceiling := s.env.ReleasedSchema().Lower(remoteHead.hold)
	if err := refuseSchemaAboveCeiling(remote, branch, remoteHead, commitSchema, ceiling); err != nil {
		return err
	}
	if err := refuseSchemaRaise(remote, branch, remoteHead, commitSchema, s.env.RaisePolicy()); err != nil {
		return err
	}
	return refuseSchemaLowering(remote, branch, remoteHead, commitSchema, commitHold)
}

// headCommitWithinLock reads HEAD's hash under the commit lock the caller
// holds, so the value is the HEAD the caller's push or check ran against.
func (s *Store) headCommitWithinLock(ctx context.Context) (string, error) {
	var head string
	if err := s.db.QueryRowContext(ctx, `SELECT commit_hash FROM dolt_log() LIMIT 1`).Scan(&head); err != nil {
		return "", fmt.Errorf("read head commit: %w", err)
	}
	return strings.TrimSpace(head), nil
}

// SyncPushFromClone pushes from a store that is a frozen clone of a live one,
// and re-checks a refusal against the remote before reporting it. See the
// storage.Syncer contract. The re-check is the fetch retryPushAfterFollow runs
// on a refusal, then a freshness read on this store — the clone, which no
// third party writes to or reads the tracking ref of, so nothing lands between
// the refusal and the verdict. A fetch that fails leaves the refusal
// standing, with the failed check joined to it: neither is dropped. A push
// that failed without a refusal fetched nothing, so there is nothing to judge
// it against and it is reported as it failed. A refusal over a re-root the
// clone follows pushes the clone's re-rooted history once more, and the live
// store follows the same re-root when it receives it.
// [LAW:no-silent-failure] [LAW:verifiable-goals] the goal is "the remote
// carries HEAD", and a rejected push is judged by the goal, not by itself.
// A push the re-check found superseded landed too, and keeps the bound.
func (s *Store) SyncPushFromClone(ctx, boundLifetime context.Context, remote storage.RemoteName, branch string, window storage.BoundWindow, setUpstream bool, force bool) (storage.SyncPushResult, error) {
	pushed, err := s.pushFromClone(ctx, remote, branch, setUpstream, force)
	return s.keepHistoryBoundAfter(boundLifetime, remote, branch, window, pushed, err)
}

// pushFromClone is SyncPushFromClone up to the push landing.
func (s *Store) pushFromClone(ctx context.Context, remote storage.RemoteName, branch string, setUpstream bool, force bool) (storage.SyncPushResult, error) {
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return storage.SyncPushResult{}, err
	}
	result, pushErr := s.pushMutation(ctx, remote, trimmedBranch, setUpstream, force)
	// The clone collects nothing below the cut: it is deleted when the
	// mirror's cycle ends, and the live store collects when it follows the
	// same re-root.
	result, pushErr, followErr := s.retryPushAfterFollow(ctx, remote, trimmedBranch, setUpstream, result, pushErr)
	if pushErr == nil {
		return result, nil
	}
	if refusal := (pushRefusedError{}); !errors.As(pushErr, &refusal) {
		return storage.SyncPushResult{}, pushErr
	}
	if followErr != nil {
		return storage.SyncPushResult{}, fmt.Errorf("%w (and whether a concurrent push superseded it could not be checked — %v)", pushErr, followErr)
	}
	err = s.runSyncMutation(ctx, func(ctx context.Context) error {
		fresh, freshErr := s.SyncFreshness(ctx, remote, trimmedBranch)
		if freshErr != nil {
			return fmt.Errorf("%w (and whether a concurrent push superseded it could not be checked: %v)", pushErr, freshErr)
		}
		if !fresh.RemoteCarriesHead() {
			return pushErr
		}
		head, headErr := s.headCommitWithinLock(ctx)
		if headErr != nil {
			return fmt.Errorf("%w (superseded by a concurrent push, but the head could not be read: %v)", pushErr, headErr)
		}
		result = storage.SyncPushResult{Head: head, Superseded: pushErr}
		return nil
	})
	if err != nil {
		return storage.SyncPushResult{}, err
	}
	return result, nil
}

func (s *Store) SyncStatus(ctx context.Context) (storage.SyncStatusReport, error) {
	report := storage.SyncStatusReport{}
	if err := s.db.QueryRowContext(ctx, `SELECT DOLT_VERSION()`).Scan(&report.EngineVersion); err != nil {
		return storage.SyncStatusReport{}, fmt.Errorf("read dolt version: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT ACTIVE_BRANCH()`).Scan(&report.Branch); err != nil {
		return storage.SyncStatusReport{}, fmt.Errorf("read active branch: %w", err)
	}
	var headMessage sql.NullString
	headQuery := `SELECT commit_hash, message FROM dolt_log() LIMIT 1`
	if err := s.db.QueryRowContext(ctx, headQuery).Scan(&report.HeadCommit, &headMessage); err != nil {
		return storage.SyncStatusReport{}, fmt.Errorf("read head commit: %w", err)
	}
	report.HeadMessage = nullStringValue(headMessage)
	remotes, err := s.SyncListRemotes(ctx)
	if err != nil {
		return storage.SyncStatusReport{}, err
	}
	report.Remotes = remotes

	rows, err := s.db.QueryContext(ctx, `SELECT table_name, staged, status FROM dolt_status ORDER BY table_name, staged`)
	if err != nil {
		return storage.SyncStatusReport{}, fmt.Errorf("read dolt status: %w", err)
	}
	defer rows.Close()

	report.Status = []storage.SyncStatusRow{}
	for rows.Next() {
		var statusRow storage.SyncStatusRow
		if err := rows.Scan(&statusRow.TableName, &statusRow.Staged, &statusRow.Status); err != nil {
			return storage.SyncStatusReport{}, fmt.Errorf("scan dolt status row: %w", err)
		}
		report.Status = append(report.Status, statusRow)
	}
	if err := rows.Err(); err != nil {
		return storage.SyncStatusReport{}, fmt.Errorf("iterate dolt status rows: %w", err)
	}
	return report, nil
}

// SyncFreshness computes the local data branch's position relative to the
// remote-tracking ref for the given remote+branch, as of the last fetch/push.
// It is a pure read against local refs — it never touches the network — so it
// runs on any open store, including doctor's read-only one. The caller resolves
// remote and branch (the same selection `lit sync` uses) and owns the
// no-remote-configured case; this method owns the never-synced case, guarding
// the range queries so they never run against a missing ref.
func (s *Store) SyncFreshness(ctx context.Context, remote storage.RemoteName, branch string) (storage.SyncFreshness, error) {
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return storage.SyncFreshness{}, err
	}
	// A branch that never synced has no base: the base's source starts at
	// none, so every freshness says where its base came from.
	// [LAW:types-are-the-program]
	freshness := storage.SyncFreshness{Remote: remote.String(), Branch: trimmedBranch, Base: storage.ReconcileBase{Source: storage.ReconcileBaseNone}}
	trackingRef := fmt.Sprintf("remotes/%s/%s", remote, trimmedBranch)

	tracked, err := trackingRefExists(ctx, s.db, trackingRef)
	if err != nil {
		return storage.SyncFreshness{}, err
	}
	if !tracked {
		// [LAW:no-defensive-null-guards] Absent tracking ref is a real domain
		// state (never synced), so it is returned as a value the caller matches
		// on — not papered over. The range queries below would error against a
		// missing ref, so they must not run here.
		return freshness, nil
	}
	freshness.Synced = true

	var localBranch string
	if err := s.db.QueryRowContext(ctx, `SELECT ACTIVE_BRANCH()`).Scan(&localBranch); err != nil {
		return storage.SyncFreshness{}, fmt.Errorf("read active branch: %w", err)
	}

	localHead, err := readDoltHead(ctx, s.db)
	if err != nil {
		return storage.SyncFreshness{}, err
	}
	remoteHead, err := commitHashOfRef(ctx, s.db, trackingRef)
	if err != nil {
		return storage.SyncFreshness{}, err
	}
	freshness.RemoteHead = remoteHead
	freshness.Base, err = s.reconcileBase(ctx, remote, trimmedBranch, localHead, remoteHead)
	if err != nil {
		return storage.SyncFreshness{}, err
	}
	// Ahead is the folded chain the reconcile would replay, so it is spelled
	// with the same exclusions: the remote head and, when there is one, the
	// base. [LAW:one-source-of-truth]
	aheadExcludes := []string{trackingRef}
	if baseCommit, found := freshness.Base.Found(); found {
		aheadExcludes = append(aheadExcludes, baseCommit)
	}
	ahead, aheadOldest, err := s.commitRangeStats(ctx, localBranch, aheadExcludes...)
	if err != nil {
		return storage.SyncFreshness{}, fmt.Errorf("summarize commits ahead of %q: %w", trackingRef, err)
	}
	behind, behindOldest, err := s.commitRangeStats(ctx, trackingRef, localBranch)
	if err != nil {
		return storage.SyncFreshness{}, fmt.Errorf("summarize commits behind %q: %w", trackingRef, err)
	}
	freshness.Ahead = ahead
	freshness.Behind = behind
	// OldestDivergedUnix dates a DIVERGENCE, so it is populated only when the
	// branch is actually diverged. An ahead-only or behind-only branch is not
	// diverged, so it stays 0 rather than carrying a timestamp its state
	// contradicts (the field name would otherwise lie on the wire). When
	// diverged, the earlier of the two ranges' oldest commits dates the fork.
	// [LAW:types-are-the-program] the field is populated iff the state it
	// names holds.
	//
	// Under a fork-point base, Behind is the remote's whole re-rooted history,
	// since the local branch holds none of its hashes. For a fork below the
	// cut that is exactly the commits past the base. For a fork above it,
	// which reaches this base only when the follow refused, it also counts the
	// re-root's copies of the commits between the cut and the base: a copy has
	// its own hash, and nothing locally held tells it from a new commit.
	if freshness.State() == storage.SyncDiverged {
		freshness.OldestDivergedUnix = earlierValidUnix(aheadOldest, behindOldest)
	}
	return freshness, nil
}

// trackingRefExists reports whether the store holds the remote-tracking ref
// trackingRef: false is the never-synced state, which no range or merge-base
// query over the ref can be asked about.
func trackingRefExists(ctx context.Context, db *sql.DB, trackingRef string) (bool, error) {
	var count int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dolt_remote_branches WHERE name = ?`, trackingRef,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check remote-tracking ref %q: %w", trackingRef, err)
	}
	return count > 0, nil
}

// earlierValidUnix returns the smaller of two optional Unix timestamps, treating
// an invalid (NULL, i.e. empty range) value as absent. Zero when neither is set.
func earlierValidUnix(a, b sql.NullInt64) int64 {
	switch {
	case a.Valid && b.Valid:
		if a.Int64 < b.Int64 {
			return a.Int64
		}
		return b.Int64
	case a.Valid:
		return a.Int64
	case b.Valid:
		return b.Int64
	default:
		return 0
	}
}

// commitRangeStats summarizes the commits reachable from head but from none of
// excluded — foldedChainQuery's spelling: how many there are, and the Unix
// time of the OLDEST one (NULL/invalid when the range is empty).
// [LAW:single-enforcer] Ahead and behind are the same query in opposite
// directions, so they share one path, and the oldest-commit date rides along
// rather than costing a second query. UNIX_TIMESTAMP(MIN(date)) yields a numeric
// scalar, not the DATETIME itself — so no time.Time crosses the driver boundary;
// the driver renders that scalar as a fractional decimal STRING (the `date` column
// is Datetime3, millisecond precision), which is why it is scanned as a NullString
// and parsed to whole seconds by parseUnixSeconds rather than into a NullInt64
// directly. The revisions are bound parameters, not interpolated, so ref names
// cannot inject SQL.
func (s *Store) commitRangeStats(ctx context.Context, head string, excluded ...string) (int64, sql.NullInt64, error) {
	var count int64
	var oldestRaw sql.NullString
	query, args := foldedChainQuery("COUNT(*), UNIX_TIMESTAMP(MIN(date))", head, excluded...)
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count, &oldestRaw); err != nil {
		return 0, sql.NullInt64{}, err
	}
	oldest, err := parseUnixSeconds(oldestRaw)
	if err != nil {
		return 0, sql.NullInt64{}, fmt.Errorf("parse oldest commit time %q: %w", oldestRaw.String, err)
	}
	return count, oldest, nil
}

// parseUnixSeconds converts the driver's UNIX_TIMESTAMP rendering into whole Unix
// seconds. The `date` column is Datetime3 (millisecond precision), so the driver
// returns UNIX_TIMESTAMP(date) as a fractional decimal STRING, e.g.
// "1784998962.153", and NULL for an empty range. The sub-second part is dropped:
// divergence age is reasoned about in hours and days, so milliseconds are noise.
// [LAW:no-silent-failure] a malformed value is an error, not a silent zero.
func parseUnixSeconds(raw sql.NullString) (sql.NullInt64, error) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return sql.NullInt64{}, nil
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(raw.String), 64)
	if err != nil {
		return sql.NullInt64{}, err
	}
	return sql.NullInt64{Int64: int64(secs), Valid: true}, nil
}

func (s *Store) runSyncMutation(ctx context.Context, operation retryOperation) error {
	return s.withCommitLock(ctx, func(ctx context.Context) error {
		return retryTransientGCContention(ctx, s.stderr, operation, s.reconnect)
	})
}

func callIntProcedure(ctx context.Context, db *sql.DB, procedure string, args ...string) (int64, error) {
	query := buildProcedureCall(procedure, len(args))
	var status int64
	if err := db.QueryRowContext(ctx, query, stringArgsToAny(args)...).Scan(&status); err != nil {
		return 0, err
	}
	return status, nil
}

// execProcedureDiscard runs a CALL whose result row carries no value this caller
// needs (e.g. DOLT_MERGE's hash/fast_forward/conflicts/message tuple) and
// surfaces only the procedure's error. [LAW:no-silent-failure] The row is
// drained and rows.Err is checked so a procedure failure reported mid-stream is
// not swallowed. It is column-count agnostic, so it does not break when Dolt's
// procedure result shape changes across versions.
func execProcedureDiscard(ctx context.Context, db *sql.DB, procedure string, args ...string) error {
	query := buildProcedureCall(procedure, len(args))
	rows, err := db.QueryContext(ctx, query, stringArgsToAny(args)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

func buildProcedureCall(procedure string, argCount int) string {
	if argCount == 0 {
		return fmt.Sprintf("CALL %s()", procedure)
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", argCount), ",")
	return fmt.Sprintf("CALL %s(%s)", procedure, placeholders)
}

func stringArgsToAny(args []string) []any {
	values := make([]any, len(args))
	for idx, arg := range args {
		values[idx] = arg
	}
	return values
}

func nullStringValue(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return strings.TrimSpace(value.String)
}

func requireSyncArg(field string, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	return trimmed, nil
}

func requireEmbeddedSyncSupport() error {
	versions := readEmbeddedModuleVersions()
	if len(versions) == 0 {
		return nil
	}
	return validateEmbeddedSyncSupport(versions)
}

func readEmbeddedModuleVersions() map[string]string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	versions := map[string]string{}
	for _, dep := range info.Deps {
		versions[dep.Path] = dep.Version
	}
	return versions
}

func validateEmbeddedSyncSupport(versions map[string]string) error {
	requirements := map[string]string{
		"github.com/dolthub/dolt/go": minEmbeddedDoltVersion,
		"github.com/dolthub/driver":  minEmbeddedDriverVersion,
	}
	for modulePath, minimumVersion := range requirements {
		actualVersion := strings.TrimSpace(versions[modulePath])
		if actualVersion == "" {
			continue
		}
		if semver.Compare(actualVersion, minimumVersion) < 0 {
			return fmt.Errorf(
				"embedded sync requires %s %s or newer (found %s)",
				modulePath,
				minimumVersion,
				actualVersion,
			)
		}
	}
	return nil
}
