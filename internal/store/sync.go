package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltenv "github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"golang.org/x/mod/semver"
)

const minEmbeddedDoltVersion = "v0.40.5-0.20260314011441-62975ef6bf36"
const minEmbeddedDriverVersion = "v0.2.1-0.20260314000741-0fe74e7ee31a"

func OpenSync(ctx context.Context, doltRootDir string, workspaceID string) (_ *Store, err error) {
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
	release, err := acquireWorkspaceShared(ctx, doltRootDir)
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
	bootstrap, err := ensureDoltDatabase(ctx, doltRootDir, workspaceID)
	if err != nil {
		return nil, err
	}
	// OpenSync is the on-change mirror's own engine open, so this is exactly
	// the call site that must wait out an earlier foreground command's (or
	// earlier mirror's) still-live engine instead of colliding with it — the
	// wait happens inside the eager engine open, on Dolt's own journal lock,
	// bounded by coResidentHolderWait.
	s, err := openStoreConnection(ctx, doltRootDir, workspaceID, engineWrite)
	if err != nil {
		return nil, err
	}
	s.releaseWorkspaceLock = release
	// The sync engine writes — merges, replays, resolutions, compactions — so
	// it meets the floor a write rule sets, as the migrating open does. It
	// applies no migration: a workspace behind this binary is the migrating
	// open's to bring forward. [LAW:single-enforcer] both write opens apply
	// the one floor, refuseUnwritable.
	_, op, err := s.assessOperability(ctx)
	if err == nil {
		err = s.refuseUnwritable(ctx, op)
	}
	s.migrated = storage.Migrated{Bootstrap: bootstrap, SchemaAhead: op.schemaAhead()}
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
// wrote belong to its owner (refOwner), so they go with it only when that
// owner names no other remote Dolt holds: for origin, origin's refs; for
// team/origin alone, team's; for a/b beside a, none, as they are a's
// branches b/*, kept until a's next pruning fetch like any of a's stale
// branches. The drops go first and the remove last: a retried attempt reruns
// the whole body, and dropping refs repeats safely where a second remove
// fails. [LAW:no-ambient-temporal-coupling]
func (s *Store) SyncRemoveRemote(ctx context.Context, name string) error {
	trimmedName, err := requireSyncArg("remote", name)
	if err != nil {
		return err
	}
	owner, err := ownerOfHeld(trimmedName)
	if err != nil {
		return err
	}
	return s.runSyncMutation(ctx, func(ctx context.Context) error {
		var ownerHeld int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_remotes WHERE name = ? AND name <> ?`, owner.String(), trimmedName).Scan(&ownerHeld); err != nil {
			return fmt.Errorf("remove dolt remote %q: read whether %q remains: %w", trimmedName, owner, err)
		}
		if ownerHeld == 0 {
			dropped, err := s.dropForkPoints(ctx, owner, nil)
			s.forkPoints = append(s.forkPoints, dropped...)
			if err != nil {
				return err
			}
			if err := s.dropTrackingRefs(ctx, owner); err != nil {
				return err
			}
		}
		if _, err := callIntProcedure(ctx, s.db, "DOLT_REMOTE", "remove", trimmedName); err != nil {
			return fmt.Errorf("remove dolt remote %q: %w", trimmedName, err)
		}
		return nil
	})
}

// SyncSetRemoteURL re-points a remote in one mutation. Dolt has no set-url, so
// it is a remove and an add; the fork points stay, because the name still
// names the remote they record, and the next fetch brings its tracking refs
// back (dropOrphanForkPoints runs only after that fetch). The remove runs only
// while the remote is there, so an attempt retried after its remove landed
// goes on to the add.
func (s *Store) SyncSetRemoteURL(ctx context.Context, name storage.RemoteName, url string) error {
	trimmedURL, err := requireSyncArg("remote url", url)
	if err != nil {
		return err
	}
	return s.runSyncMutation(ctx, func(ctx context.Context) error {
		var present int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_remotes WHERE name = ?`, name.String()).Scan(&present); err != nil {
			return fmt.Errorf("re-point dolt remote %q: read it: %w", name, err)
		}
		if present > 0 {
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

func (s *Store) SyncFetch(ctx context.Context, remote storage.RemoteName, prune bool) error {
	args := []string{remote.String()}
	if prune {
		args = append([]string{"--prune"}, args...)
	}
	return s.runSyncMutation(ctx, func(ctx context.Context) error {
		// The fork points are recorded in the same mutation as the fetch and
		// ahead of it, because the fetch moves every tracking ref of the remote
		// however the remote's history moved (recordForkPoints). Every attempt
		// records, so a retried attempt reports what it found as well.
		forkPoints, err := s.recordForkPoints(ctx, ownerOf(remote))
		s.forkPoints = append(s.forkPoints, forkPoints...)
		if err != nil {
			return err
		}
		// [LAW:single-enforcer] Remote transport retry wraps the one network-touching
		// call, inside the mutation's own GC-contention retry: the two loops recover
		// unrelated failures under separate budgets (see remote_io.go).
		err = runRemoteIO(ctx, func(ctx context.Context) error {
			_, err := callIntProcedure(ctx, s.db, "DOLT_FETCH", args...)
			return err
		})
		if err != nil {
			return fmt.Errorf("fetch remote %q: %w", remote, err)
		}
		dropped, err := s.dropOrphanForkPoints(ctx, ownerOf(remote))
		s.forkPoints = append(s.forkPoints, dropped...)
		return err
	})
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
	if err := s.SyncFetch(ctx, remote, false); err != nil {
		return storage.SyncPullResult{}, err
	}
	if _, err := s.followRemoteReRoot(ctx, remote, branch, storage.SyncEntryPull); err != nil {
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
// reset primitive shared by SyncResetToRemoteHead (the `lit init` adopt) and the
// unrelated-histories take-remote resolution, so the two cannot drift on how the
// wholesale adopt is spelled. [LAW:single-enforcer] The ref is bound by the caller
// to a real tracking ref; this owns only the reset.
func resetHardToRef(ctx context.Context, db *sql.DB, ref string) error {
	if _, err := callIntProcedure(ctx, db, "DOLT_RESET", "--hard", ref); err != nil {
		return fmt.Errorf("reset to remote head %q: %w", ref, err)
	}
	return nil
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

	if err := s.SyncFetch(ctx, remote, false); err != nil {
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
	if _, err := s.followRemoteReRoot(ctx, remote, trimmedBranch, storage.SyncEntryReceive); err != nil {
		return storage.SyncReceiveResult{}, err
	}
	return s.settleReceived(ctx, remote, trimmedBranch)
}

// settleReceived runs settleReceivedWithinLock as a sync mutation, joining a
// commit lock the caller already holds.
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
	return result, nil
}

// strandedWritesCommitMessage is the message of the commit a sync lands over
// writes a failed DOLT_COMMIT left in the working set.
const strandedWritesCommitMessage = "commit writes a failed commit left staged"

// freshnessOverEveryWrite is the freshness that decides whether the branch
// moves. Freshness counts commits, and landing on the remote head or on a
// reconcile's replay resets the working set, so writes a failed DOLT_COMMIT
// left staged are versioned first, as the next mutation would version them.
// Then the freshness counts every local write: a stranded write is a local
// commit, and the branch is diverged, not behind. [LAW:single-enforcer] The
// settle and the reconcile plan both read it. [LAW:no-silent-failure]
func (s *Store) freshnessOverEveryWrite(ctx context.Context, remote storage.RemoteName, branch string) (storage.SyncFreshness, error) {
	var tables int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT table_name) FROM dolt_status`).Scan(&tables); err != nil {
		return storage.SyncFreshness{}, fmt.Errorf("read the working set: %w", err)
	}
	if err := s.commitWorkingSetOnce(ctx, commitStamp{Message: strandedWritesCommitMessage}); err != nil {
		return storage.SyncFreshness{}, err
	}
	if tables > 0 {
		s.strandedWritesCommits = append(s.strandedWritesCommits, storage.StrandedWritesCommit{Tables: tables})
	}
	return s.SyncFreshness(ctx, remote, branch)
}

// settleReceivedWithinLock is the half of a receive that follows the fetch:
// the post-fetch freshness selects the outcome, and the one state that moves
// the branch (behind) lands on the remote head. SyncReceive and
// SyncSettleReceived both run it, so a receive whose fetch ran here and one
// whose fetch ran on a clone settle identically. [LAW:single-enforcer] The
// caller holds the commit lock.
func (s *Store) settleReceivedWithinLock(ctx context.Context, remote storage.RemoteName, branch string) (storage.SyncReceiveResult, error) {
	var result storage.SyncReceiveResult
	fresh, err := s.freshnessOverEveryWrite(ctx, remote, branch)
	if err != nil {
		return result, err
	}
	result.Ahead, result.Behind = fresh.Ahead, fresh.Behind
	result.OldestDivergedUnix = fresh.OldestDivergedUnix
	result.Base = fresh.Base.Source
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
// The sweep is here, after the reconnect, because this is the one moment
// orphaned instance refs can be told from live ones: the reconnect just closed
// every remote database this process held, nothing has opened one since, and
// Dolt's LOCK and the commit lock keep every other writer of the mirrors —
// another write engine, a receive's landing — off the store. So every path that
// compacts collects the refs, and none can sweep without that moment.
// [LAW:no-ambient-temporal-coupling] A sweep that fails reports in its outcome
// rather than failing the compaction that already rewrote the store.
func (s *Store) compactWithinLock(ctx context.Context, mode GCMode) (compactionAttempt, refSweepOutcome, error) {
	attempt := compactionAttempt{Depth: mode}
	args, err := gcProcedureArgs(mode)
	if err != nil {
		return attempt, refSweepOutcome{}, err
	}
	if _, err := callIntProcedure(ctx, s.db, "DOLT_GC", args...); err != nil {
		return attempt, refSweepOutcome{}, fmt.Errorf("compact dolt store (%s): %w", mode, err)
	}
	// Recorded here, before the reconnect below and not after it: DOLT_GC has
	// returned, so the store on disk is already rewritten and nothing that
	// follows can un-rewrite it. A reconnect failure must not read as a pass
	// that never happened. [LAW:no-silent-failure]
	attempt.Ran = true
	// [LAW:single-enforcer] Online GC poisons the active SQL connection; the Store rotates it here so every downstream query contract is restored before lock release.
	if err := s.reconnect(ctx); err != nil {
		return attempt, refSweepOutcome{}, err
	}
	return attempt, s.sweepOrphanedInstanceRefs(ctx), nil
}

// SyncCompact compacts the store at the requested depth, with no remote
// involved. It is what a workspace that never pushes has to reach for, and the
// depth is the caller's to choose.
//
// The before/after footprint is measured around the pass and returned already
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
	before, beforeErr := s.measureFootprint()
	var attempt compactionAttempt
	var sweep refSweepOutcome
	// [LAW:single-enforcer] Dolt garbage collection is exposed through a single Store entrypoint so every caller routes through the same commit-lock and retry wrapper.
	err := s.runSyncMutation(ctx, func(ctx context.Context) error {
		var swept refSweepOutcome
		var compactErr error
		attempt, swept, compactErr = s.compactWithinLock(ctx, mode)
		sweep = sweep.plus(swept)
		return compactErr
	})
	// Measured on both paths, because a pass that ran and then failed to
	// reconnect rewrote the store exactly as much as one that succeeded. The
	// reading survives a dead connection: measureFootprint is one Stat and one
	// ReadDir against the filesystem, with no engine and no lock involved.
	after, afterErr := s.measureFootprint()
	// The sweep's line joins the footprint's: both are this pass's account of
	// what it changed on disk, and Detail is where that account goes.
	outcome := attempt.outcome(joinMaintenance(footprintDelta(before, after, errors.Join(beforeErr, afterErr)), sweep.Report()))
	if err != nil {
		// The outcome is returned populated ALONGSIDE the error, because it
		// describes the compaction and the compaction may well have happened —
		// compactWithinLock's last act is a reconnect, and a failure there
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

// CompactIfDue compacts only when this store's own footprint says a pass is
// owed. It is the backstop's entrypoint: the judgment lives here, beside the
// layout it reads, so a cadence owner never has to know what a journal is.
// [LAW:decomposition]
//
// "Nothing was due" returns a zero outcome and no error, because it is the
// ordinary result — the check is cheap and meant to be asked often, while the
// pass is neither.
func (s *Store) CompactIfDue(ctx context.Context) (storage.CompactionOutcome, error) {
	footprint, err := s.measureFootprint()
	if err != nil {
		// "I could not measure" is not "nothing is due". Returning the error
		// keeps an unreadable store from quietly ceasing to be maintained.
		// [LAW:no-silent-failure]
		return storage.CompactionOutcome{}, fmt.Errorf("measure store footprint: %w", err)
	}
	mode, due := dueMode(footprint)
	if !due {
		return storage.CompactionOutcome{}, nil
	}
	return s.SyncCompact(ctx, mode)
}

// chooseCompactionDepth picks the depth for the push path. The push always
// compacts at least the new generation — that is this path's contract and it
// does not become conditional on a measurement succeeding — so the footprint
// can only ever deepen the pass, never cancel it.
//
// A measurement that fails therefore returns a usable depth AND its error: the
// fallback is this path's contract, the new generation alone, which makes it a
// safe floor rather than an invented one, and the caller reports the problem
// instead of the store silently choosing for it.
// [LAW:no-silent-failure]
func (s *Store) chooseCompactionDepth() (GCMode, error) {
	footprint, err := s.measureFootprint()
	if err != nil {
		return GCNewGen, err
	}
	if mode, due := dueMode(footprint); due {
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
		return fmt.Errorf("%w (and whether the remote re-rooted its history could not be checked: %v)", pushErr, followErr)
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

// pushRefusalTexts are the texts DOLT_PUSH's two refusals carry. The SQL
// boundary renders DOLT_PUSH's error as text (MySQL error 1105), so a refusal
// is recognized by its text, as pushLeasedWithinLock recognizes a stale
// lease.
//   - The first is the hint of env.ErrFailedToPush, which Dolt returns only
//     when a target was rejected non-fast-forward. Its opening line is not
//     used: it is git's own "failed to push some refs" line, which a git
//     remote's rejected write carries too. Git gives this hint only for a
//     non-fast-forward rejection, and a git remote's writes are all forced.
//     TestPushRefusalTextsAreDoltsOwn holds it to Dolt's message
//     [LAW:one-source-of-truth].
//   - The second is a re-rooted remote, which shares no ancestor with the
//     push.
//
// Both come from the fast-forward check, which a forced push skips, so a
// forced push is never refused.
var pushRefusalTexts = []string{pushRejectedHint, pushNoAncestorText}

const pushRejectedHint = "hint: Updates were rejected because the tip of your current branch is behind"

var pushNoAncestorText = doltdb.ErrNoCommonAncestor.Error()

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
	text := pushErr.Error()
	if slices.ContainsFunc(pushRefusalTexts, func(refusal string) bool { return strings.Contains(text, refusal) }) {
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
// re-root the fetch revealed (followRemoteReRoot). When the follow moved the
// branch, the refusal was the re-root's and the follow removed it, so the
// push runs once more, as a plain push, and that push's outcome is the
// result. Otherwise the refusal stands, returned as pushErr with the fetch
// already run, so a caller judging it reads the remote as it stands now. A
// refusal that stands, the first push's or the retry's, comes back judged by
// judgeStandingRefusal: every push step's refusal passes through here, so one
// judgment covers them all. [LAW:single-enforcer]
// followErr is a fetch or follow that failed, beside the refusal it could not
// explain, for the caller to word; followed is the follow's record, zero when
// the branch did not move, for the caller's maintenance account. Every other
// outcome — a push that landed,
// or one that failed without a refusal (an unreachable remote, a commit lock
// not acquired, a compaction that failed first) — passes through untouched
// with nothing fetched.
func (s *Store) retryPushAfterFollow(ctx context.Context, remote storage.RemoteName, branch string, setUpstream bool, pushed storage.SyncPushResult, pushErr error) (result storage.SyncPushResult, _ error, followed storage.FollowedReRoot, followErr error) {
	trimmedBranch := strings.TrimSpace(branch)
	var refusal pushRefusedError
	if !errors.As(pushErr, &refusal) {
		return pushed, pushErr, storage.FollowedReRoot{}, nil
	}
	if err := s.SyncFetch(ctx, remote, false); err != nil {
		return storage.SyncPushResult{}, pushErr, storage.FollowedReRoot{}, err
	}
	followed, err := s.followRemoteReRoot(ctx, remote, trimmedBranch, storage.SyncEntryPush)
	if err != nil {
		return storage.SyncPushResult{}, pushErr, followed, err
	}
	if followed.Head != "" {
		result, pushErr = s.pushMutation(ctx, remote, trimmedBranch, setUpstream, false)
		if !errors.As(pushErr, &refusal) {
			return result, pushErr, followed, nil
		}
		// The retry was refused over a remote that moved after the first
		// fetch, so its judgment reads the remote as it stands now too.
		if err := s.SyncFetch(ctx, remote, false); err != nil {
			return storage.SyncPushResult{}, unjudgedRefusal(pushErr, err), followed, nil
		}
	}
	return storage.SyncPushResult{}, s.judgeStandingRefusal(ctx, remote, trimmedBranch, pushErr), followed, nil
}

// judgeStandingRefusal is a refusal that stands, after its fetch, wrapped in
// storage.ErrPushDiverged when the branch and the remote each hold commits
// the other lacks.
func (s *Store) judgeStandingRefusal(ctx context.Context, remote storage.RemoteName, branch string, refusal error) error {
	fresh, err := s.SyncFreshness(ctx, remote, branch)
	if err != nil {
		return unjudgedRefusal(refusal, err)
	}
	if fresh.CommitsDiverged() {
		return fmt.Errorf("%w: %w", storage.ErrPushDiverged, refusal)
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
// not one method with a compaction flag. [LAW:decomposition] Neither keeps the
// history bound: the bound follows whichever push landed, through
// SyncKeepHistoryBound.
func (s *Store) SyncCompactAndPush(ctx context.Context, remote storage.RemoteName, branch string, setUpstream bool, force bool) (storage.SyncPushResult, error) {
	// The branch is refused before the compaction, which rewrites the store
	// under the commit lock, not after it in the push.
	branch, err := requireSyncArg("branch", branch)
	if err != nil {
		return storage.SyncPushResult{}, err
	}
	var attempt compactionAttempt
	var depthErr error
	var sweep refSweepOutcome
	var result storage.SyncPushResult
	err = s.runSyncMutation(ctx, func(ctx context.Context) error {
		// Measured inside the lock, where no other mutation can grow the
		// journal between the reading and the pass it selects.
		var depth GCMode
		depth, depthErr = s.chooseCompactionDepth()
		var swept refSweepOutcome
		var compactErr error
		attempt, swept, compactErr = s.compactWithinLock(ctx, depth)
		sweep = sweep.plus(swept)
		if compactErr != nil {
			return compactErr
		}
		var pushErr error
		result, pushErr = s.pushWithinLock(ctx, remote, branch, setUpstream, force)
		return pushErr
	})
	// A push refused over a re-root it follows runs again without
	// compacting: the follow collected the store below the cut, and that
	// collection joins the maintenance account.
	result, err, followed, followErr := s.retryPushAfterFollow(ctx, remote, branch, setUpstream, result, err)
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
		collectionReport(followed.Collection),
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
	// [LAW:single-enforcer] Refuse before authoring a commit onto a remote
	// whose head is at a schema this binary cannot produce — otherwise a plain
	// push is rejected with a raw non-fast-forward string and a --force push
	// would REGRESS the shared remote to this binary's older schema.
	if err := s.guardRemoteSchemaAhead(ctx, remote, trimmedBranch); err != nil {
		return storage.SyncPushResult{}, err
	}
	args := []string{}
	if setUpstream {
		args = append(args, "--set-upstream")
	}
	if force {
		args = append(args, "--force")
	}
	args = append(args, remote.String(), fmt.Sprintf("HEAD:%s", trimmedBranch))
	// HEAD is read BEFORE the push: the caller holds the commit lock, so HEAD
	// cannot move between this read and the push, and a read that fails
	// fails an attempt that has sent nothing. Read afterwards, a failure
	// (the push landing just inside its deadline, the read just outside)
	// would report a landed push as failed. [LAW:no-ambient-temporal-coupling]
	head, err := s.headCommitWithinLock(ctx)
	if err != nil {
		return storage.SyncPushResult{}, err
	}
	query := buildProcedureCall("DOLT_PUSH", len(args))
	var result storage.SyncPushResult
	var message sql.NullString
	// One dropped SSH connection must not fail the whole push: the transport
	// retry absorbs it, and only a failure that survives the budget reaches the
	// caller — as RemoteUnreachableError naming the transport symptom, never as
	// the backend's misrendered authentication failure.
	pushErr := runRemoteIO(ctx, func(ctx context.Context) error {
		return s.db.QueryRowContext(ctx, query, stringArgsToAny(args)...).Scan(&result.Status, &message)
	})
	if pushErr != nil {
		return storage.SyncPushResult{}, classifyPushFailure(remote, pushErr)
	}
	result.Message = nullStringValue(message)
	result.Head = head
	return result, nil
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
func (s *Store) SyncPushFromClone(ctx context.Context, remote storage.RemoteName, branch string, setUpstream bool, force bool) (storage.SyncPushResult, error) {
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return storage.SyncPushResult{}, err
	}
	result, pushErr := s.pushMutation(ctx, remote, trimmedBranch, setUpstream, force)
	result, pushErr, _, followErr := s.retryPushAfterFollow(ctx, remote, trimmedBranch, setUpstream, result, pushErr)
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
		result = storage.SyncPushResult{Head: head, Superseded: pushErr.Error()}
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
	freshness := storage.SyncFreshness{Remote: remote.String(), Branch: trimmedBranch}
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
		return retryTransientGCContention(ctx, operation, s.reconnect)
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
