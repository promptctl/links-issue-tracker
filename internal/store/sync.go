package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"

	doltenv "github.com/dolthub/dolt/go/libraries/doltcore/env"
	"golang.org/x/mod/semver"
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

type SyncPullResult struct {
	FastForward int64  `json:"fast_forward"`
	Conflicts   int64  `json:"conflicts"`
	Message     string `json:"message"`
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
	// [LAW:single-enforcer] Sync bootstrap reuses the Store database initializer so first-run sync and regular store opens share one creation boundary.
	if _, err = EnsureDatabase(ctx, doltRootDir, workspaceID); err != nil {
		return nil, err
	}
	s, err := openStoreConnection(doltRootDir, workspaceID)
	if err != nil {
		return nil, err
	}
	s.releaseWorkspaceLock = release
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

func (s *Store) SyncPull(ctx context.Context, remote string, branch string) (SyncPullResult, error) {
	trimmedRemote, err := requireSyncArg("remote", remote)
	if err != nil {
		return SyncPullResult{}, err
	}
	trimmedBranch := strings.TrimSpace(branch)
	args := []string{trimmedRemote}
	if trimmedBranch != "" {
		args = append(args, trimmedBranch)
	}

	var result SyncPullResult
	err = s.runSyncMutation(ctx, func(ctx context.Context) error {
		query := buildProcedureCall("DOLT_PULL", len(args))
		var message sql.NullString
		err := s.db.QueryRowContext(ctx, query, stringArgsToAny(args)...).Scan(&result.FastForward, &result.Conflicts, &message)
		if err != nil {
			return fmt.Errorf("pull remote %q: %w", trimmedRemote, err)
		}
		result.Message = nullStringValue(message)
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
		if _, err := callIntProcedure(ctx, s.db, "DOLT_RESET", "--hard", trackingRef); err != nil {
			return fmt.Errorf("reset to remote head %q: %w", trackingRef, err)
		}
		return nil
	})
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
// was decided from.
type SyncReceiveResult struct {
	State  SyncReceiveState
	Ahead  int64
	Behind int64
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
	return s.reconnect()
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

	ahead, err := s.countCommitRange(ctx, trackingRef, localBranch)
	if err != nil {
		return SyncFreshness{}, fmt.Errorf("count commits ahead of %q: %w", trackingRef, err)
	}
	behind, err := s.countCommitRange(ctx, localBranch, trackingRef)
	if err != nil {
		return SyncFreshness{}, fmt.Errorf("count commits behind %q: %w", trackingRef, err)
	}
	freshness.Ahead = ahead
	freshness.Behind = behind
	return freshness, nil
}

// countCommitRange counts commits reachable from `to` but not from `from` — the
// dolt_log two-dot range `from..to`. [LAW:single-enforcer] Ahead and behind are
// the same query in opposite directions, so they share one path. The range is a
// bound parameter, not interpolated, so ref names cannot inject SQL.
func (s *Store) countCommitRange(ctx context.Context, from string, to string) (int64, error) {
	var count int64
	rangeExpr := fmt.Sprintf("%s..%s", from, to)
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dolt_log(?)`, rangeExpr).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
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
