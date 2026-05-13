package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Checkpoint is a named anchor in Dolt history (a branch ref) that callers
// can reset back to. The CreatedAt is recovered from the Name's nanosecond
// suffix, so a Checkpoint round-trips through dolt_branches without needing a
// side table to remember when it was made.
type Checkpoint struct {
	Name      string
	Prefix    string
	CreatedAt time.Time
	CommitSHA string
}

// checkpointPrefixPattern restricts prefixes to a stable shape so the
// "<prefix>-<unix-nanos>" naming round-trips through ListCheckpoints
// unambiguously and keeps Dolt branch names safe (lowercase + hyphen, no
// embedded separators or shell-meaningful characters).
var checkpointPrefixPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// CreateCheckpoint creates a Dolt branch from current HEAD with the shape
// "<prefix>-<unix-nanos>" and prunes prior same-prefix checkpoints down to
// `retain` newest. Caller must hold the commit lock so concurrent mutations
// cannot interleave with the branch creation.
//
// [LAW:single-enforcer] This file is the only writer of checkpoint-shaped
// Dolt branch lifecycle (create / reset / list / prune); callers route
// through this surface rather than calling DOLT_BRANCH directly.
// [LAW:dataflow-not-control-flow] Same operations on every call: branch
// create, then prune. The prefix and retain count are data, not branches.
func (s *Store) CreateCheckpoint(ctx context.Context, prefix string, retain int) (Checkpoint, error) {
	if !checkpointPrefixPattern.MatchString(prefix) {
		return Checkpoint{}, fmt.Errorf("checkpoint prefix %q is invalid (must match %s)", prefix, checkpointPrefixPattern)
	}
	if retain < 0 {
		return Checkpoint{}, fmt.Errorf("checkpoint retain must be non-negative, got %d", retain)
	}
	now := time.Now().UTC()
	name := formatCheckpointName(prefix, now)
	if _, err := s.db.ExecContext(ctx, "CALL DOLT_BRANCH(?)", name); err != nil {
		return Checkpoint{}, fmt.Errorf("create checkpoint branch %q: %w", name, err)
	}
	sha, err := readBranchHash(ctx, s.db, name)
	if err != nil {
		return Checkpoint{}, err
	}
	cp := Checkpoint{Name: name, Prefix: prefix, CreatedAt: now, CommitSHA: sha}
	if err := s.PruneCheckpoints(ctx, prefix, retain); err != nil {
		return cp, err
	}
	return cp, nil
}

// ResetToCheckpoint hard-resets the active branch (master) to the named
// checkpoint. Caller must hold the commit lock; the reset moves HEAD and
// clears any unstaged working set, so any concurrent reader would otherwise
// see torn state.
func (s *Store) ResetToCheckpoint(ctx context.Context, name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("checkpoint name is required")
	}
	if _, err := s.db.ExecContext(ctx, "CALL DOLT_RESET('--hard', ?)", trimmed); err != nil {
		return fmt.Errorf("reset to checkpoint %q: %w", trimmed, err)
	}
	return nil
}

// ListCheckpoints returns every checkpoint with the given prefix, newest
// first. Branches whose suffix does not parse as a unix-nanos timestamp are
// silently skipped — only this file authors checkpoint names, so a
// non-parsing suffix means the branch came from elsewhere and is not part of
// the checkpoint set.
func (s *Store) ListCheckpoints(ctx context.Context, prefix string) ([]Checkpoint, error) {
	if !checkpointPrefixPattern.MatchString(prefix) {
		return nil, fmt.Errorf("checkpoint prefix %q is invalid (must match %s)", prefix, checkpointPrefixPattern)
	}
	pattern := prefix + "-%"
	rows, err := s.db.QueryContext(ctx,
		"SELECT name, hash FROM dolt_branches WHERE name LIKE ?", pattern)
	if err != nil {
		return nil, fmt.Errorf("query checkpoints with prefix %q: %w", prefix, err)
	}
	defer rows.Close()
	checkpoints := make([]Checkpoint, 0)
	for rows.Next() {
		var name, hash string
		if err := rows.Scan(&name, &hash); err != nil {
			return nil, fmt.Errorf("scan checkpoint row: %w", err)
		}
		cp, ok := parseCheckpointName(name, prefix, hash)
		if !ok {
			continue
		}
		checkpoints = append(checkpoints, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate checkpoint rows: %w", err)
	}
	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].CreatedAt.After(checkpoints[j].CreatedAt)
	})
	return checkpoints, nil
}

// PruneCheckpoints deletes every checkpoint with the given prefix beyond the
// `retain` newest. Force-deletion (-D) is required because checkpoint
// branches are not merged into master in normal use — they are point-in-time
// anchors, and refusing to delete unmerged branches would leave the prune a
// no-op forever.
func (s *Store) PruneCheckpoints(ctx context.Context, prefix string, retain int) error {
	if retain < 0 {
		return fmt.Errorf("checkpoint retain must be non-negative, got %d", retain)
	}
	checkpoints, err := s.ListCheckpoints(ctx, prefix)
	if err != nil {
		return err
	}
	for i := retain; i < len(checkpoints); i++ {
		victim := checkpoints[i].Name
		if _, err := s.db.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", victim); err != nil {
			return fmt.Errorf("delete checkpoint %q: %w", victim, err)
		}
	}
	return nil
}

func formatCheckpointName(prefix string, t time.Time) string {
	return fmt.Sprintf("%s-%d", prefix, t.UTC().UnixNano())
}

func parseCheckpointName(name, prefix, sha string) (Checkpoint, bool) {
	expected := prefix + "-"
	if !strings.HasPrefix(name, expected) {
		return Checkpoint{}, false
	}
	suffix := name[len(expected):]
	nanos, err := strconv.ParseInt(suffix, 10, 64)
	if err != nil || nanos < 0 {
		return Checkpoint{}, false
	}
	return Checkpoint{
		Name:      name,
		Prefix:    prefix,
		CreatedAt: time.Unix(0, nanos).UTC(),
		CommitSHA: sha,
	}, true
}

func readBranchHash(ctx context.Context, db *sql.DB, name string) (string, error) {
	var hash string
	if err := db.QueryRowContext(ctx,
		"SELECT hash FROM dolt_branches WHERE name = ?", name).Scan(&hash); err != nil {
		return "", fmt.Errorf("read branch %q hash: %w", name, err)
	}
	return hash, nil
}
