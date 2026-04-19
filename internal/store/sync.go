package store

import (
	"context"
	"database/sql"
	"fmt"
	"runtime/debug"
	"strings"

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

func OpenSync(ctx context.Context, doltRootDir string, workspaceID string) (*Store, error) {
	if strings.TrimSpace(doltRootDir) == "" {
		return nil, fmt.Errorf("dolt root dir is required")
	}
	if strings.TrimSpace(workspaceID) == "" {
		return nil, fmt.Errorf("workspace id is required")
	}
	if err := requireEmbeddedSyncSupport(); err != nil {
		return nil, err
	}
	// [LAW:single-enforcer] Sync bootstrap reuses the Store database initializer so first-run sync and regular store opens share one creation boundary.
	if err := EnsureDatabase(ctx, doltRootDir, workspaceID); err != nil {
		return nil, err
	}
	return openStoreConnection(doltRootDir, workspaceID)
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

func (s *Store) SyncPush(ctx context.Context, remote string, branch string, setUpstream bool, force bool) (SyncPushResult, error) {
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

	var result SyncPushResult
	err = s.runSyncMutation(ctx, func(ctx context.Context) error {
		// [LAW:dataflow-not-control-flow] Every push unconditionally compacts first; gc decides what to reclaim from store state, not a caller-supplied gate.
		// [LAW:single-enforcer] Compact + push run under one commit-lock acquisition so no other mutation can interleave between GC and push.
		if err := s.compactWithinLock(ctx); err != nil {
			return err
		}
		query := buildProcedureCall("DOLT_PUSH", len(args))
		var message sql.NullString
		err := s.db.QueryRowContext(ctx, query, stringArgsToAny(args)...).Scan(&result.Status, &message)
		if err != nil {
			return fmt.Errorf("push remote %q: %w", trimmedRemote, err)
		}
		result.Message = nullStringValue(message)
		return nil
	})
	if err != nil {
		return SyncPushResult{}, err
	}
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

func (s *Store) runSyncMutation(ctx context.Context, operation retryOperation) error {
	return s.withCommitLock(ctx, func(ctx context.Context) error {
		return retryTransientManifestReadOnly(ctx, operation, transientManifestRetryDelay, waitWithContext)
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
