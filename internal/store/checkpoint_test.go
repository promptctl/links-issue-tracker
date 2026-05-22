package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestCheckpointCreateAndList pins that CreateCheckpoint creates a Dolt branch
// that appears in ListCheckpoints with the correct prefix filter.
func TestCheckpointCreateAndList(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	before, err := st.ListCheckpoints(ctx, "pre-migrate")
	if err != nil {
		t.Fatalf("ListCheckpoints before create error = %v", err)
	}

	cp, err := st.CreateCheckpoint(ctx, "pre-migrate")
	if err != nil {
		t.Fatalf("CreateCheckpoint() error = %v", err)
	}
	if cp.Name == "" {
		t.Fatal("Checkpoint.Name is empty")
	}
	if cp.CommitSHA == "" {
		t.Fatal("Checkpoint.CommitSHA is empty")
	}
	if cp.Prefix != "pre-migrate" {
		t.Errorf("Checkpoint.Prefix = %q, want %q", cp.Prefix, "pre-migrate")
	}
	if cp.CreatedAt.IsZero() {
		t.Fatal("Checkpoint.CreatedAt is zero")
	}

	after, err := st.ListCheckpoints(ctx, "pre-migrate")
	if err != nil {
		t.Fatalf("ListCheckpoints after create error = %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("ListCheckpoints count = %d, want %d (before=%d + 1)", len(after), len(before)+1, len(before))
	}

	var found bool
	for _, listed := range after {
		if listed.Name == cp.Name {
			found = true
			if listed.CommitSHA != cp.CommitSHA {
				t.Errorf("listed CommitSHA = %q, want %q", listed.CommitSHA, cp.CommitSHA)
			}
		}
	}
	if !found {
		t.Errorf("created checkpoint %q not found in ListCheckpoints", cp.Name)
	}
}

// TestCheckpointListExcludesOtherPrefixes pins that ListCheckpoints returns
// only branches matching the given prefix and no cross-prefix contamination.
func TestCheckpointListExcludesOtherPrefixes(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	// Use a test-only prefix that Open does not touch, so we own the baseline count.
	beforeA, err := st.ListCheckpoints(ctx, "test-alpha")
	if err != nil {
		t.Fatalf("ListCheckpoints(test-alpha) before error = %v", err)
	}
	beforeB, err := st.ListCheckpoints(ctx, "test-beta")
	if err != nil {
		t.Fatalf("ListCheckpoints(test-beta) before error = %v", err)
	}

	if _, err := st.CreateCheckpoint(ctx, "test-alpha"); err != nil {
		t.Fatalf("CreateCheckpoint(test-alpha) error = %v", err)
	}
	if _, err := st.CreateCheckpoint(ctx, "test-beta"); err != nil {
		t.Fatalf("CreateCheckpoint(test-beta) error = %v", err)
	}

	alphaCPs, err := st.ListCheckpoints(ctx, "test-alpha")
	if err != nil {
		t.Fatalf("ListCheckpoints(test-alpha) error = %v", err)
	}
	if len(alphaCPs) != len(beforeA)+1 {
		t.Errorf("ListCheckpoints(test-alpha) count = %d, want %d", len(alphaCPs), len(beforeA)+1)
	}

	betaCPs, err := st.ListCheckpoints(ctx, "test-beta")
	if err != nil {
		t.Fatalf("ListCheckpoints(test-beta) error = %v", err)
	}
	if len(betaCPs) != len(beforeB)+1 {
		t.Errorf("ListCheckpoints(test-beta) count = %d, want %d", len(betaCPs), len(beforeB)+1)
	}
}

// TestCheckpointResetReverts pins that ResetToCheckpoint rolls back committed
// changes made after the checkpoint was taken.
func TestCheckpointResetReverts(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	// Seed a row that should survive the reset.
	if err := st.ExecRawForTest(ctx,
		`INSERT INTO meta(meta_key, meta_value) VALUES ('persist', 'yes')`,
	); err != nil {
		t.Fatalf("seed persistent row error = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "seed persistent row"); err != nil {
		t.Fatalf("commit persistent row error = %v", err)
	}

	// Create the checkpoint here; the persistent row is in the checkpoint state.
	cp, err := st.CreateCheckpoint(ctx, "test-cp")
	if err != nil {
		t.Fatalf("CreateCheckpoint() error = %v", err)
	}

	// Insert a row that should be erased by the reset.
	if err := st.ExecRawForTest(ctx,
		`INSERT INTO meta(meta_key, meta_value) VALUES ('transient', 'gone')`,
	); err != nil {
		t.Fatalf("seed transient row error = %v", err)
	}
	if err := st.commitWorkingSet(ctx, "seed transient row"); err != nil {
		t.Fatalf("commit transient row error = %v", err)
	}

	// Verify transient row exists before reset.
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE meta_key = 'transient'`).Scan(&count); err != nil {
		t.Fatalf("count transient row error = %v", err)
	}
	if count != 1 {
		t.Fatalf("transient row count before reset = %d, want 1", count)
	}

	// Reset to checkpoint.
	if err := st.ResetToCheckpoint(ctx, cp.Name); err != nil {
		t.Fatalf("ResetToCheckpoint() error = %v", err)
	}

	// Transient row should be gone.
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE meta_key = 'transient'`).Scan(&count); err != nil {
		t.Fatalf("count transient row after reset error = %v", err)
	}
	if count != 0 {
		t.Errorf("transient row count after reset = %d, want 0 (reset did not revert)", count)
	}

	// Persistent row should still be present.
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE meta_key = 'persist'`).Scan(&count); err != nil {
		t.Fatalf("count persistent row after reset error = %v", err)
	}
	if count != 1 {
		t.Errorf("persistent row count after reset = %d, want 1 (reset erased pre-checkpoint data)", count)
	}
}

// TestCheckpointPruneEnforcesRetention pins that PruneCheckpoints reduces the
// branch count to at most `retain` for the given prefix.
func TestCheckpointPruneEnforcesRetention(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	const total = 7
	const retain = 3
	var names []string
	for i := 0; i < total; i++ {
		// Space calls out to ensure unique nanosecond timestamps.
		time.Sleep(time.Millisecond)
		cp, err := st.CreateCheckpoint(ctx, "cp-test")
		if err != nil {
			t.Fatalf("CreateCheckpoint(%d) error = %v", i, err)
		}
		names = append(names, cp.Name)
	}

	before, err := st.ListCheckpoints(ctx, "cp-test")
	if err != nil {
		t.Fatalf("ListCheckpoints before prune error = %v", err)
	}
	if len(before) != total {
		t.Fatalf("before prune count = %d, want %d", len(before), total)
	}

	if err := st.PruneCheckpoints(ctx, "cp-test", retain); err != nil {
		t.Fatalf("PruneCheckpoints() error = %v", err)
	}

	after, err := st.ListCheckpoints(ctx, "cp-test")
	if err != nil {
		t.Fatalf("ListCheckpoints after prune error = %v", err)
	}
	if len(after) != retain {
		t.Fatalf("after prune count = %d, want %d", len(after), retain)
	}

	// The retained branches should be the newest `retain` ones.
	retained := map[string]bool{}
	for _, cp := range after {
		retained[cp.Name] = true
	}
	for i, name := range names {
		wantKept := i >= total-retain
		if retained[name] != wantKept {
			t.Errorf("checkpoint[%d] %q: retained=%v, want %v", i, name, retained[name], wantKept)
		}
	}
}

// TestCheckpointPruneZeroDeletesAll pins that PruneCheckpoints(retain=0)
// deletes every checkpoint for the given prefix.
func TestCheckpointPruneZeroDeletesAll(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	for i := 0; i < 3; i++ {
		time.Sleep(time.Millisecond)
		if _, err := st.CreateCheckpoint(ctx, "cp-zero-test"); err != nil {
			t.Fatalf("CreateCheckpoint(%d) error = %v", i, err)
		}
	}

	if err := st.PruneCheckpoints(ctx, "cp-zero-test", 0); err != nil {
		t.Fatalf("PruneCheckpoints(0) error = %v", err)
	}

	remaining, err := st.ListCheckpoints(ctx, "cp-zero-test")
	if err != nil {
		t.Fatalf("ListCheckpoints after prune(0) error = %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("remaining checkpoint count = %d, want 0", len(remaining))
	}
}

// TestCheckpointSortedOldestFirst pins that ListCheckpoints returns checkpoints
// in creation order (oldest first).
func TestCheckpointSortedOldestFirst(t *testing.T) {
	ctx := context.Background()
	doltRoot := filepath.Join(t.TempDir(), "dolt")

	st, err := Open(ctx, doltRoot, "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	var created []string
	for i := 0; i < 4; i++ {
		time.Sleep(time.Millisecond)
		cp, err := st.CreateCheckpoint(ctx, "sorted-test")
		if err != nil {
			t.Fatalf("CreateCheckpoint(%d) error = %v", i, err)
		}
		created = append(created, cp.Name)
	}

	listed, err := st.ListCheckpoints(ctx, "sorted-test")
	if err != nil {
		t.Fatalf("ListCheckpoints error = %v", err)
	}
	if len(listed) != len(created) {
		t.Fatalf("listed count = %d, want %d", len(listed), len(created))
	}
	for i, cp := range listed {
		if cp.Name != created[i] {
			t.Errorf("position %d: got %q, want %q", i, cp.Name, created[i])
		}
	}
}

// TestParseCheckpointName pins the naming round-trip: a name produced by
// CreateCheckpoint's formatting is reconstructed identically by
// parseCheckpointName.
func TestParseCheckpointName(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		wantOk bool
	}{
		{"pre-migrate-1716998765000000000", "pre-migrate", true},
		{"other-123456789", "other", true},
		{"pre-migrate-abc", "pre-migrate", false},   // suffix not numeric
		{"pre-migrate-", "pre-migrate", false},       // empty suffix
		{"not-matching-123", "pre-migrate", false},   // wrong prefix
		{"pre-migrate-123", "other", false},           // wrong prefix arg
		{"pre-migrate", "pre-migrate", false},         // no suffix
	}
	for _, c := range cases {
		cp, ok := parseCheckpointName(c.name, c.prefix)
		if ok != c.wantOk {
			t.Errorf("parseCheckpointName(%q, %q) ok=%v, want %v", c.name, c.prefix, ok, c.wantOk)
		}
		if ok && cp.Name != c.name {
			t.Errorf("parseCheckpointName(%q, %q).Name = %q, want %q", c.name, c.prefix, cp.Name, c.name)
		}
		if ok && cp.Prefix != c.prefix {
			t.Errorf("parseCheckpointName(%q, %q).Prefix = %q, want %q", c.name, c.prefix, cp.Prefix, c.prefix)
		}
	}
}
