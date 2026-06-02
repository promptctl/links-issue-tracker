package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Checkpoint is a named Dolt branch created at a specific HEAD commit to
// provide a lightweight revert point for migration failures. Not
// migration-specific — any Store operation that needs a Dolt-native rollback
// anchor can use a different prefix and reuse this primitive.
//
// [LAW:types-are-the-program] The type encodes the full description of a
// revert point: name, prefix, creation time, and the commit it was created
// at. The name encodes the prefix and timestamp so ListCheckpoints can
// reconstruct the set without external metadata storage.
type Checkpoint struct {
	Name      string    // "<prefix>-<unix-nano>"
	Prefix    string    // caller label, e.g. "pre-migrate"
	CreatedAt time.Time // parsed from the unix-nano suffix in Name
	CommitSHA string    // Dolt HEAD commit hash at creation time
}

// CreateCheckpoint creates a Dolt branch at the current HEAD and returns the
// resulting Checkpoint.
//
// [LAW:single-enforcer] All Dolt branching for migration checkpoints routes
// through this method; no other code calls DOLT_BRANCH directly for this.
func (s *Store) CreateCheckpoint(ctx context.Context, prefix string) (Checkpoint, error) {
	// [LAW:one-source-of-truth] CommitSHA is captured from the one HEAD reader at
	// creation so the caller has a stable reference independent of later branch
	// movement.
	commitSHA, err := readDoltHead(ctx, s.db)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("checkpoint: %w", err)
	}
	ts := time.Now().UTC()
	name := fmt.Sprintf("%s-%d", prefix, ts.UnixNano())
	if _, err := s.db.ExecContext(ctx, "CALL DOLT_BRANCH(?)", name); err != nil {
		return Checkpoint{}, fmt.Errorf("checkpoint: create branch %q: %w", name, err)
	}
	return Checkpoint{
		Name:      name,
		Prefix:    prefix,
		CreatedAt: ts,
		CommitSHA: commitSHA,
	}, nil
}

// ResetToCheckpoint hard-resets the current branch to the commit the named
// checkpoint branch points to, discarding all working-set changes and any
// Dolt commits made after the checkpoint was taken.
//
// [LAW:single-enforcer] All Dolt hard-resets for migration recovery route
// through this method.
func (s *Store) ResetToCheckpoint(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx, "CALL DOLT_RESET('--hard', ?)", name); err != nil {
		return fmt.Errorf("checkpoint: reset to %q: %w", name, err)
	}
	return nil
}

// ListCheckpoints returns all checkpoint branches whose name matches
// "<prefix>-<unix-nano>", sorted oldest first.
func (s *Store) ListCheckpoints(ctx context.Context, prefix string) ([]Checkpoint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, hash FROM dolt_branches WHERE name LIKE ? ORDER BY name`,
		prefix+"-%",
	)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: list branches: %w", err)
	}
	defer rows.Close()
	var cps []Checkpoint
	for rows.Next() {
		var name, hash string
		if err := rows.Scan(&name, &hash); err != nil {
			return nil, fmt.Errorf("checkpoint: scan branch: %w", err)
		}
		cp, ok := parseCheckpointName(name, prefix)
		if !ok {
			continue
		}
		cp.CommitSHA = hash
		cps = append(cps, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checkpoint: iterate branches: %w", err)
	}
	sort.Slice(cps, func(i, j int) bool { return cps[i].CreatedAt.Before(cps[j].CreatedAt) })
	return cps, nil
}

// PruneCheckpoints deletes the oldest checkpoint branches for the given
// prefix until at most retain branches remain. retain=0 deletes all.
func (s *Store) PruneCheckpoints(ctx context.Context, prefix string, retain int) error {
	if retain < 0 {
		return fmt.Errorf("checkpoint: retain must be non-negative, got %d", retain)
	}
	cps, err := s.ListCheckpoints(ctx, prefix)
	if err != nil {
		return err
	}
	if len(cps) <= retain {
		return nil
	}
	for _, cp := range cps[:len(cps)-retain] {
		if _, err := s.db.ExecContext(ctx, "CALL DOLT_BRANCH('-d', '-f', ?)", cp.Name); err != nil {
			return fmt.Errorf("checkpoint: delete branch %q: %w", cp.Name, err)
		}
	}
	return nil
}

// parseCheckpointName reconstructs a Checkpoint from a branch name. The name
// must be "<prefix>-<unix-nano>"; returns false if the format doesn't match.
func parseCheckpointName(name, prefix string) (Checkpoint, bool) {
	needle := prefix + "-"
	if len(name) <= len(needle) || name[:len(needle)] != needle {
		return Checkpoint{}, false
	}
	var ns int64
	suffix := name[len(needle):]
	if _, err := fmt.Sscanf(suffix, "%d", &ns); err != nil || fmt.Sprintf("%d", ns) != suffix {
		return Checkpoint{}, false
	}
	return Checkpoint{
		Name:      name,
		Prefix:    prefix,
		CreatedAt: time.Unix(0, ns).UTC(),
	}, true
}
