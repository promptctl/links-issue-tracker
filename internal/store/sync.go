package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"

	doltenv "github.com/dolthub/dolt/go/libraries/doltcore/env"
	"golang.org/x/mod/semver"

	"github.com/promptctl/links-issue-tracker/internal/merge"
)

type SyncRemote struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

const minEmbeddedDoltVersion = "v0.40.5-0.20260314011441-62975ef6bf36"
const minEmbeddedDriverVersion = "v0.2.1-0.20260314000741-0fe74e7ee31a"

type SyncStatusRow struct {
	TableName string `json:"table_name"`
	Staged    bool   `json:"staged"`
	Status    string `json:"status"`
}

type SyncStatusReport struct {
	DoltVersion string          `json:"dolt_version"`
	Branch      string          `json:"branch"`
	HeadCommit  string          `json:"head_commit"`
	HeadMessage string          `json:"head_message"`
	Status      []SyncStatusRow `json:"status"`
	Remotes     []SyncRemote    `json:"remotes"`
}

// SyncPullState classifies what a single `lit sync pull` did, derived from the
// receive (fetch + fast-forward) and, when the branch diverged, the field-aware
// reconcile. [LAW:one-source-of-truth] one mapping from the underlying
// receive/reconcile outcomes to a pull label; the CLI renders this, it never
// re-derives it.
type SyncPullState string

const (
	// SyncPullUpToDate: local already at the remote head; nothing to pull.
	SyncPullUpToDate SyncPullState = "up_to_date"
	// SyncPullFastForwarded: local was strictly behind and advanced to the
	// remote head with no merge commit.
	SyncPullFastForwarded SyncPullState = "fast_forwarded"
	// SyncPullLinearized: local diverged and the field-aware reconcile merged
	// the divergence into linear history — the same outcome the automatic
	// receive produces, reached explicitly here.
	SyncPullLinearized SyncPullState = "linearized"
	// SyncPullProsePending: local diverged and every code-owned field settled,
	// but a free-text field diverged on both sides. Nothing is committed; the
	// prose conflicts are held for the agent surface. [LAW:no-silent-failure]
	SyncPullProsePending SyncPullState = "prose_pending"
	// SyncPullUnrelated: local diverged from a remote it shares no history with, so
	// the reconcile found no common ancestor and merged nothing. Nothing is
	// committed; the divergence is surfaced for wholesale/union resolution rather
	// than merged through an absent base. [LAW:no-silent-failure]
	SyncPullUnrelated SyncPullState = "unrelated_histories"
	// SyncPullAhead: local has unpushed commits and the remote has nothing new;
	// there is nothing to pull (push delivers local commits).
	SyncPullAhead SyncPullState = "ahead"
	// SyncPullNeverSynced: the remote has no ref for this branch even after a
	// fetch — the branch has never been pushed, so there is nothing to pull.
	SyncPullNeverSynced SyncPullState = "never_synced"
)

// SyncPullResult reports the pull outcome, the ahead/behind counts it was
// decided from, and — for SyncPullProsePending only — the free-text conflicts
// held for the agent surface.
type SyncPullResult struct {
	State   SyncPullState        `json:"state"`
	Ahead   int64                `json:"ahead"`
	Behind  int64                `json:"behind"`
	Pending []merge.ProsePending `json:"pending,omitempty"`
	// OldestDivergedUnix dates the divergence this pull observed (Unix seconds),
	// so a held-conflict surface can escalate by age. Zero unless the pull met a
	// divergence. Carried from the receive that classified the freshness.
	OldestDivergedUnix int64 `json:"oldest_diverged_unix,omitempty"`
	// Unrelated carries the both-sides issue-id partition, non-nil only for
	// SyncPullUnrelated. Carried straight off the reconcile that detected the
	// no-common-ancestor divergence, so the pull surface enumerates the same
	// partition `lit sync reconcile` does. [LAW:one-source-of-truth]
	Unrelated *UnrelatedInventory `json:"unrelated,omitempty"`
}

type SyncPushResult struct {
	Status  int64  `json:"status"`
	Message string `json:"message"`
}

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
	if _, err = ensureDoltDatabase(ctx, doltRootDir, workspaceID); err != nil {
		return nil, err
	}
	// OpenSync is the on-change mirror's own engine open, so this is exactly
	// the call site that must wait out an earlier foreground command's (or
	// earlier mirror's) still-live engine instead of colliding with it
	// (links-sync-pgct.11) — the wait happens inside the eager engine open,
	// on Dolt's own journal lock, bounded by engineOpenRetryMaxElapsed.
	s, err := openStoreConnection(ctx, doltRootDir, workspaceID, engineWrite)
	if err != nil {
		return nil, err
	}
	s.releaseWorkspaceLock = release
	// Same per-open branch normalization Store.Open runs: the bootstrap only
	// normalizes at creation now, so a pre-made database (an adopt's clone)
	// gets renamed to master here on the mirror's own engine rather than by
	// a bootstrap pool. Unconditional; a normalized store reads and does
	// nothing. [LAW:dataflow-not-control-flow]
	// [LAW:single-enforcer] The commit lock is the single writer gate for
	// every mutation on a live write store — the rename joins it here just
	// as it does in Open, so a snapshot copy or a concurrent read-open's
	// migration can never interleave with an in-flight branch rename.
	if err = s.withCommitLock(ctx, func(ctx context.Context) error {
		return ensureMasterDefaultBranch(ctx, s.db)
	}); err != nil {
		err = wrapEngineOpenContention(err)
		if closeErr := s.db.Close(); closeErr != nil && !errors.Is(closeErr, context.Canceled) {
			err = errors.Join(err, closeErr)
		}
		s.releaseWorkspaceLock = nil
		return nil, err
	}
	success = true
	return s, nil
}

func (s *Store) SyncListRemotes(ctx context.Context) ([]SyncRemote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, url FROM dolt_remotes ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list dolt remotes: %w", err)
	}
	defer rows.Close()

	remotes := []SyncRemote{}
	for rows.Next() {
		var remote SyncRemote
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

func (s *Store) SyncAddRemote(ctx context.Context, name string, url string) error {
	// [LAW:single-enforcer] Sync input normalization is enforced once at the Store boundary so every caller shares the same contract.
	trimmedName, err := requireSyncArg("remote name", name)
	if err != nil {
		return err
	}
	trimmedURL, err := requireSyncArg("remote url", url)
	if err != nil {
		return err
	}
	return s.runSyncMutation(ctx, func(ctx context.Context) error {
		_, err := callIntProcedure(ctx, s.db, "DOLT_REMOTE", "add", trimmedName, trimmedURL)
		if err != nil {
			return fmt.Errorf("add dolt remote %q: %w", trimmedName, err)
		}
		return nil
	})
}

func (s *Store) SyncRemoveRemote(ctx context.Context, name string) error {
	trimmedName, err := requireSyncArg("remote name", name)
	if err != nil {
		return err
	}
	return s.runSyncMutation(ctx, func(ctx context.Context) error {
		_, err := callIntProcedure(ctx, s.db, "DOLT_REMOTE", "remove", trimmedName)
		if err != nil {
			return fmt.Errorf("remove dolt remote %q: %w", trimmedName, err)
		}
		return nil
	})
}

func (s *Store) SyncFetch(ctx context.Context, remote string, prune bool) error {
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return err
	}
	args := []string{trimmedRemote}
	if prune {
		args = append([]string{"--prune"}, args...)
	}
	return s.runSyncMutation(ctx, func(ctx context.Context) error {
		_, err := callIntProcedure(ctx, s.db, "DOLT_FETCH", args...)
		if err != nil {
			return fmt.Errorf("fetch remote %q: %w", trimmedRemote, err)
		}
		return nil
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
// It composes SyncReceive (fetch + fast-forward + freshness) and SyncReconcile
// rather than reimplementing either; the divergence branch is the only one that
// reconciles, and every other freshness state carries straight through.
// [LAW:dataflow-not-control-flow]
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
// The whole converge runs under ONE commit lock: acquireCommitLock is
// context-reentrant, so SyncReceive's and SyncReconcile's own acquisitions
// short-circuit and the two steps are atomic against every other writer. That
// closes the TOCTOU window in which a concurrent push, landing between an
// independently-locked receive and reconcile, would make the local strictly
// behind — the reconcile would then read NotDiverged and the pull would report
// the contradictory "up to date, N behind". Freshness is computed from local
// refs that cannot move under the held lock, so the diverged state the receive
// saw is the state the reconcile heals. [LAW:no-ambient-temporal-coupling] one
// owner of the converge's atomicity.
func (s *Store) SyncPull(ctx context.Context, remote string, branch string) (SyncPullResult, error) {
	var result SyncPullResult
	err := s.withCommitLock(ctx, func(ctx context.Context) error {
		recv, err := s.SyncReceive(ctx, remote, branch)
		if err != nil {
			return err
		}
		result.Ahead, result.Behind = recv.Ahead, recv.Behind
		result.OldestDivergedUnix = recv.OldestDivergedUnix
		switch recv.State {
		case SyncReceiveUpToDate:
			result.State = SyncPullUpToDate
		case SyncReceiveFastForwarded:
			result.State = SyncPullFastForwarded
		case SyncReceiveAhead:
			result.State = SyncPullAhead
		case SyncReceiveNeverSynced:
			result.State = SyncPullNeverSynced
		case SyncReceiveDiverged:
			rec, err := s.SyncReconcile(ctx, remote, branch)
			if err != nil {
				return err
			}
			result.Ahead, result.Behind = rec.Ahead, rec.Behind
			switch rec.State {
			case SyncReconcileLinearized:
				result.State = SyncPullLinearized
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
			case SyncReconcileProsePending:
				result.State = SyncPullProsePending
				result.Pending = rec.Pending
			case SyncReconcileUnrelated:
				// No common ancestor: nothing merged, nothing committed. The divergence
				// is real and still present, so the ahead/behind counts and the fork
				// timestamp the receive recorded ride along unchanged — the fork the
				// receive saw is the fork this reports. [LAW:one-source-of-truth] The
				// both-sides inventory the reconcile read off the two anchors rides along
				// too, so the pull surface shows what each side holds.
				result.State = SyncPullUnrelated
				result.Unrelated = rec.Unrelated
			case SyncReconcileNotDiverged:
				// Under the single lock a push race cannot resolve the divergence
				// between the receive and the reconcile, so this is the benign
				// idempotent case (the divergence was already gone). Nothing to merge.
				result.State = SyncPullUpToDate
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
		return SyncPullResult{}, err
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
func (s *Store) SyncResetToRemoteHead(ctx context.Context, remote string, branch string) error {
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return err
	}
	trackingRef := fmt.Sprintf("remotes/%s/%s", trimmedRemote, trimmedBranch)
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

// SyncReceiveState classifies what a single background receive did, derived
// from the post-fetch freshness. [LAW:one-source-of-truth] One mapping from
// freshness to outcome; the CLI renders this, it never re-derives it.
type SyncReceiveState string

const (
	// SyncReceiveUpToDate: local already at the remote head; fetch found nothing.
	SyncReceiveUpToDate SyncReceiveState = "up_to_date"
	// SyncReceiveFastForwarded: local was strictly behind and advanced to the
	// remote head with no merge commit — the only state that mutates local data.
	SyncReceiveFastForwarded SyncReceiveState = "fast_forwarded"
	// SyncReceiveAhead: local has unpushed commits and the remote has nothing
	// new; there is nothing to receive (the push side delivers local commits).
	SyncReceiveAhead SyncReceiveState = "ahead"
	// SyncReceiveDiverged: both sides moved; a fast-forward is impossible and a
	// real merge is required. The background receive deliberately does NOT merge
	// here — that is the foreground, agent-present reconcile (links-multi-machine-ttde.2).
	// Reported, never silently skipped. [LAW:no-silent-failure]
	SyncReceiveDiverged SyncReceiveState = "diverged"
	// SyncReceiveNeverSynced: no remote-tracking ref even after a fetch (the
	// remote has no data on this branch yet); nothing to receive.
	SyncReceiveNeverSynced SyncReceiveState = "never_synced"
)

// SyncReceiveResult reports the receive outcome and the ahead/behind counts it
// was decided from, plus — when diverged — the Unix time the divergence began,
// so the inline reconcile that follows a SyncReceiveDiverged can date the fork
// it is about to heal without a second freshness read. Zero unless diverged.
type SyncReceiveResult struct {
	State              SyncReceiveState
	Ahead              int64
	Behind             int64
	OldestDivergedUnix int64
}

// SyncReceive fetches the remote and, only when the local branch is strictly
// behind, fast-forwards it to the remote head. It is purely lossless: it never
// creates a merge commit, never merges a divergence, and never leaves a dirty
// or conflicted working set — a fast-forward only moves a branch pointer that
// has no local commits to lose. [LAW:effects-at-boundaries] The one state that
// touches local data (behind → fast-forward) is the only safe automatic one;
// every other state is observed and reported for the caller, with the diverged
// case explicitly deferred to the foreground agent-present reconcile rather than
// silently dropped. [LAW:dataflow-not-control-flow] The post-fetch freshness is
// the value that selects the outcome; there is one fetch and one freshness read
// every call.
func (s *Store) SyncReceive(ctx context.Context, remote string, branch string) (SyncReceiveResult, error) {
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return SyncReceiveResult{}, err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return SyncReceiveResult{}, err
	}

	var result SyncReceiveResult
	err = s.runSyncMutation(ctx, func(ctx context.Context) error {
		if _, err := callIntProcedure(ctx, s.db, "DOLT_FETCH", trimmedRemote); err != nil {
			return fmt.Errorf("fetch remote %q: %w", trimmedRemote, err)
		}
		fresh, err := s.SyncFreshness(ctx, trimmedRemote, trimmedBranch)
		if err != nil {
			return err
		}
		result.Ahead, result.Behind = fresh.Ahead, fresh.Behind
		result.OldestDivergedUnix = fresh.OldestDivergedUnix
		switch fresh.State() {
		case SyncBehind:
			trackingRef := fmt.Sprintf("remotes/%s/%s", trimmedRemote, trimmedBranch)
			if err := execProcedureDiscard(ctx, s.db, "DOLT_MERGE", "--ff-only", trackingRef); err != nil {
				return fmt.Errorf("fast-forward to %q: %w", trackingRef, err)
			}
			result.State = SyncReceiveFastForwarded
		case SyncDiverged:
			result.State = SyncReceiveDiverged
		case SyncAhead:
			result.State = SyncReceiveAhead
		case SyncNeverSynced:
			result.State = SyncReceiveNeverSynced
		default:
			result.State = SyncReceiveUpToDate
		}
		return nil
	})
	if err != nil {
		return SyncReceiveResult{}, err
	}
	return result, nil
}

// compactWithinLock runs DOLT_GC and rotates the connection. The caller must
// already hold the commit lock; SyncCompact and SyncPush both compose over
// this helper so the compact step has one implementation regardless of whether
// it runs as a standalone mutation or as the first step of a larger one.
func (s *Store) compactWithinLock(ctx context.Context) error {
	if _, err := callIntProcedure(ctx, s.db, "DOLT_GC"); err != nil {
		return fmt.Errorf("compact dolt store: %w", err)
	}
	// [LAW:single-enforcer] Online GC poisons the active SQL connection; the Store rotates it here so every downstream query contract is restored before lock release.
	return s.reconnect(ctx)
}

func (s *Store) SyncCompact(ctx context.Context) error {
	// [LAW:single-enforcer] Dolt garbage collection is exposed through a single Store entrypoint so every caller routes through the same commit-lock and retry wrapper.
	return s.runSyncMutation(ctx, s.compactWithinLock)
}

// SyncPush mirrors the local branch to the remote. It only pushes — one path,
// every call, no mode bit. [LAW:dataflow-not-control-flow] Maintenance
// compaction is the separate SyncCompactAndPush entrypoint; the interactive
// on-change mirror calls this plain push because DOLT_GC transitions the
// embedded store read-only mid-run and collides with the engine state just
// after a mutation, and reclaiming local disk is not worth that on every change.
func (s *Store) SyncPush(ctx context.Context, remote string, branch string, setUpstream bool, force bool) (SyncPushResult, error) {
	var result SyncPushResult
	err := s.runSyncMutation(ctx, func(ctx context.Context) error {
		pushed, pushErr := s.pushWithinLock(ctx, remote, branch, setUpstream, force)
		result = pushed
		return pushErr
	})
	if err != nil {
		return SyncPushResult{}, err
	}
	return result, nil
}

// SyncCompactAndPush compacts then pushes under one commit-lock acquisition, so
// no other mutation interleaves between the garbage collection and the push and
// the push reflects exactly the compacted state. [LAW:no-ambient-temporal-coupling]
// The explicit `lit sync push` and the pre-push hook use this; the on-change
// mirror uses the plain SyncPush. The two are distinct single-purpose
// entrypoints, not one method with a compaction flag. [LAW:decomposition]
func (s *Store) SyncCompactAndPush(ctx context.Context, remote string, branch string, setUpstream bool, force bool) (SyncPushResult, error) {
	var result SyncPushResult
	err := s.runSyncMutation(ctx, func(ctx context.Context) error {
		if err := s.compactWithinLock(ctx); err != nil {
			return err
		}
		pushed, pushErr := s.pushWithinLock(ctx, remote, branch, setUpstream, force)
		result = pushed
		return pushErr
	})
	if err != nil {
		return SyncPushResult{}, err
	}
	return result, nil
}

// pushWithinLock runs DOLT_PUSH for the resolved remote and branch. The caller
// holds the commit lock (via runSyncMutation); SyncPush and SyncCompactAndPush
// both compose over this one push implementation so the push step cannot drift
// between them. [LAW:single-enforcer]
func (s *Store) pushWithinLock(ctx context.Context, remote string, branch string, setUpstream bool, force bool) (SyncPushResult, error) {
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return SyncPushResult{}, err
	}
	trimmedBranch := strings.TrimSpace(branch)
	// [LAW:single-enforcer] Refuse before authoring a commit onto a remote whose
	// head is at a schema this binary cannot produce — otherwise a plain push is
	// rejected with a raw non-fast-forward string (the exact ignorable line the
	// sync-skew epic kills) and a --force push would REGRESS the shared remote to
	// this binary's older schema. The guard needs the resolved tracking ref, so it
	// runs once the branch is known; an empty branch (push HEAD with no explicit
	// branch) has no tracking ref to compare against and is left to Dolt.
	if trimmedBranch != "" {
		if err := s.guardRemoteSchemaAhead(ctx, trimmedRemote, trimmedBranch); err != nil {
			return SyncPushResult{}, err
		}
	}
	args := []string{}
	if setUpstream {
		args = append(args, "--set-upstream")
	}
	if force {
		args = append(args, "--force")
	}
	args = append(args, trimmedRemote)
	if trimmedBranch != "" {
		args = append(args, fmt.Sprintf("HEAD:%s", trimmedBranch))
	}
	query := buildProcedureCall("DOLT_PUSH", len(args))
	var result SyncPushResult
	var message sql.NullString
	if err := s.db.QueryRowContext(ctx, query, stringArgsToAny(args)...).Scan(&result.Status, &message); err != nil {
		return SyncPushResult{}, fmt.Errorf("push remote %q: %w", trimmedRemote, err)
	}
	result.Message = nullStringValue(message)
	return result, nil
}

func (s *Store) SyncStatus(ctx context.Context) (SyncStatusReport, error) {
	report := SyncStatusReport{}
	if err := s.db.QueryRowContext(ctx, `SELECT DOLT_VERSION()`).Scan(&report.DoltVersion); err != nil {
		return SyncStatusReport{}, fmt.Errorf("read dolt version: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT ACTIVE_BRANCH()`).Scan(&report.Branch); err != nil {
		return SyncStatusReport{}, fmt.Errorf("read active branch: %w", err)
	}
	var headMessage sql.NullString
	headQuery := `SELECT commit_hash, message FROM dolt_log() LIMIT 1`
	if err := s.db.QueryRowContext(ctx, headQuery).Scan(&report.HeadCommit, &headMessage); err != nil {
		return SyncStatusReport{}, fmt.Errorf("read head commit: %w", err)
	}
	report.HeadMessage = nullStringValue(headMessage)
	remotes, err := s.SyncListRemotes(ctx)
	if err != nil {
		return SyncStatusReport{}, err
	}
	report.Remotes = remotes

	rows, err := s.db.QueryContext(ctx, `SELECT table_name, staged, status FROM dolt_status ORDER BY table_name, staged`)
	if err != nil {
		return SyncStatusReport{}, fmt.Errorf("read dolt status: %w", err)
	}
	defer rows.Close()

	report.Status = []SyncStatusRow{}
	for rows.Next() {
		var statusRow SyncStatusRow
		if err := rows.Scan(&statusRow.TableName, &statusRow.Staged, &statusRow.Status); err != nil {
			return SyncStatusReport{}, fmt.Errorf("scan dolt status row: %w", err)
		}
		report.Status = append(report.Status, statusRow)
	}
	if err := rows.Err(); err != nil {
		return SyncStatusReport{}, fmt.Errorf("iterate dolt status rows: %w", err)
	}
	return report, nil
}

// SyncFreshnessState classifies the local data branch's position relative to
// the remote-tracking ref. It is derived solely from whether that ref exists
// and the ahead/behind commit counts (see SyncFreshness.State), so there is one
// mapping from observation to label and no caller re-derives it.
// [LAW:one-source-of-truth]
type SyncFreshnessState string

const (
	SyncNeverSynced SyncFreshnessState = "never_synced"
	SyncUpToDate    SyncFreshnessState = "up_to_date"
	SyncAhead       SyncFreshnessState = "ahead"
	SyncBehind      SyncFreshnessState = "behind"
	SyncDiverged    SyncFreshnessState = "diverged"
)

// SyncFreshness reports the local data branch's position relative to the
// remote-tracking ref `remotes/<Remote>/<Branch>`. That ref reflects the remote
// as of the last fetch or push, so Behind is "as of last fetch" — computing it
// never contacts the network. Synced is false when the ref does not exist yet
// (the remote has never been pushed to or fetched from); Ahead and Behind are
// zero in that state, which is why State, not the raw counts, is the discriminant
// a renderer switches on.
type SyncFreshness struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	Synced bool   `json:"synced"`
	Ahead  int64  `json:"ahead"`
	Behind int64  `json:"behind"`
	// OldestDivergedUnix is the Unix time (seconds) of the OLDEST commit in the
	// union of the two divergent ranges — i.e. when this fork first happened. It
	// dates the divergence itself, so an escalation surface can distinguish a
	// fresh divergence from one that has festered for days. Zero when nothing has
	// diverged (both counts are 0) or the branch never synced. It is a raw
	// timestamp, not an age: the clock that turns it into "N hours ago" lives at
	// the rendering boundary, keeping this read a pure function of local refs.
	// [LAW:effects-at-boundaries] [LAW:types-are-the-program] a stored age would
	// let the value contradict "now"; a timestamp cannot.
	OldestDivergedUnix int64 `json:"oldest_diverged_unix"`
}

// State derives the classification from the raw observations. Keeping it a
// computed method (rather than a stored field) makes a label that contradicts
// the counts unrepresentable. [LAW:types-are-the-program]
func (f SyncFreshness) State() SyncFreshnessState {
	if !f.Synced {
		return SyncNeverSynced
	}
	switch {
	case f.Ahead == 0 && f.Behind == 0:
		return SyncUpToDate
	case f.Behind == 0:
		return SyncAhead
	case f.Ahead == 0:
		return SyncBehind
	default:
		return SyncDiverged
	}
}

// SyncFreshness computes the local data branch's position relative to the
// remote-tracking ref for the given remote+branch, as of the last fetch/push.
// It is a pure read against local refs — it never touches the network — so it
// runs on any open store, including doctor's read-only one. The caller resolves
// remote and branch (the same selection `lit sync` uses) and owns the
// no-remote-configured case; this method owns the never-synced case, guarding
// the range queries so they never run against a missing ref.
func (s *Store) SyncFreshness(ctx context.Context, remote string, branch string) (SyncFreshness, error) {
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return SyncFreshness{}, err
	}
	trimmedBranch, err := requireSyncArg("branch", branch)
	if err != nil {
		return SyncFreshness{}, err
	}
	freshness := SyncFreshness{Remote: trimmedRemote, Branch: trimmedBranch}
	trackingRef := fmt.Sprintf("remotes/%s/%s", trimmedRemote, trimmedBranch)

	var trackingRefCount int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dolt_remote_branches WHERE name = ?`, trackingRef,
	).Scan(&trackingRefCount); err != nil {
		return SyncFreshness{}, fmt.Errorf("check remote-tracking ref %q: %w", trackingRef, err)
	}
	if trackingRefCount == 0 {
		// [LAW:no-defensive-null-guards] Absent tracking ref is a real domain
		// state (never synced), so it is returned as a value the caller matches
		// on — not papered over. The range queries below would error against a
		// missing ref, so they must not run here.
		return freshness, nil
	}
	freshness.Synced = true

	var localBranch string
	if err := s.db.QueryRowContext(ctx, `SELECT ACTIVE_BRANCH()`).Scan(&localBranch); err != nil {
		return SyncFreshness{}, fmt.Errorf("read active branch: %w", err)
	}

	ahead, aheadOldest, err := s.commitRangeStats(ctx, trackingRef, localBranch)
	if err != nil {
		return SyncFreshness{}, fmt.Errorf("summarize commits ahead of %q: %w", trackingRef, err)
	}
	behind, behindOldest, err := s.commitRangeStats(ctx, localBranch, trackingRef)
	if err != nil {
		return SyncFreshness{}, fmt.Errorf("summarize commits behind %q: %w", trackingRef, err)
	}
	freshness.Ahead = ahead
	freshness.Behind = behind
	// OldestDivergedUnix dates a DIVERGENCE, so it is populated only when the
	// branch is actually diverged — commits on BOTH sides. An ahead-only or
	// behind-only branch is not diverged, so it stays 0 rather than carrying a
	// timestamp its state contradicts (the field name would otherwise lie on the
	// wire). When diverged, the two ranges partition the post-merge-base commits,
	// so the earlier of their oldest commits dates the fork. [LAW:types-are-the-program]
	// the field is populated iff the state it names holds.
	if ahead > 0 && behind > 0 {
		freshness.OldestDivergedUnix = earlierValidUnix(aheadOldest, behindOldest)
	}
	return freshness, nil
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

// commitRangeStats summarizes the commits reachable from `to` but not from
// `from` — the dolt_log two-dot range `from..to`: how many there are, and the
// Unix time of the OLDEST one (NULL/invalid when the range is empty).
// [LAW:single-enforcer] Ahead and behind are the same query in opposite
// directions, so they share one path, and the oldest-commit date rides along
// rather than costing a second query. UNIX_TIMESTAMP(MIN(date)) yields a numeric
// scalar, not the DATETIME itself — so no time.Time crosses the driver boundary;
// the driver renders that scalar as a fractional decimal STRING (the `date` column
// is Datetime3, millisecond precision), which is why it is scanned as a NullString
// and parsed to whole seconds by parseUnixSeconds rather than into a NullInt64
// directly. The range is a bound parameter, not interpolated, so ref names cannot
// inject SQL.
func (s *Store) commitRangeStats(ctx context.Context, from string, to string) (int64, sql.NullInt64, error) {
	var count int64
	var oldestRaw sql.NullString
	rangeExpr := fmt.Sprintf("%s..%s", from, to)
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), UNIX_TIMESTAMP(MIN(date)) FROM dolt_log(?)`, rangeExpr,
	).Scan(&count, &oldestRaw); err != nil {
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
		return retryTransientGCContention(ctx, operation, s.reconnect, transientRetryDelay, waitWithContext)
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
