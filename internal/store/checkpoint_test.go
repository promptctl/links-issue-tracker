package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCreateCheckpointPrunesToRetainCount drives the spec's first acceptance:
// CreateCheckpoint("test", 3) four times → only 3 most-recent branches remain.
// We check the count exactly so a future bug that leaks a prune is caught
// even when retention happens to be loose enough to hide it.
func TestCreateCheckpointPrunesToRetainCount(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	for i := 0; i < 4; i++ {
		if _, err := st.CreateCheckpoint(ctx, "test", 3); err != nil {
			t.Fatalf("CreateCheckpoint #%d error = %v", i, err)
		}
		// Distinct nano-timestamps so the prune order is deterministic.
		time.Sleep(2 * time.Millisecond)
	}

	cps, err := st.ListCheckpoints(ctx, "test")
	if err != nil {
		t.Fatalf("ListCheckpoints error = %v", err)
	}
	if len(cps) != 3 {
		t.Fatalf("expected 3 checkpoints after retain=3 prune, got %d (%v)", len(cps), checkpointNames(cps))
	}
	for i := 1; i < len(cps); i++ {
		if !cps[i-1].CreatedAt.After(cps[i].CreatedAt) {
			t.Fatalf("expected newest-first ordering; cps[%d]=%s not after cps[%d]=%s",
				i-1, cps[i-1].CreatedAt, i, cps[i].CreatedAt)
		}
	}
}

// TestResetToCheckpointRestoresHEAD covers the second acceptance: write,
// checkpoint, write more, reset → workspace HEAD matches the checkpoint and
// the post-checkpoint write is gone. Probes the issues table because that's
// the user-visible payload the checkpoint exists to protect.
func TestResetToCheckpointRestoresHEAD(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	pre, err := st.CreateIssue(ctx, CreateIssueInput{
		Title: "before-checkpoint", Topic: "test", IssueType: "task", Prefix: "links",
	})
	if err != nil {
		t.Fatalf("seed pre-checkpoint issue error = %v", err)
	}

	cp, err := withCommitLockTest(t, ctx, st, func(ctx context.Context) (Checkpoint, error) {
		return st.CreateCheckpoint(ctx, "reset-test", 5)
	})
	if err != nil {
		t.Fatalf("CreateCheckpoint error = %v", err)
	}

	post, err := st.CreateIssue(ctx, CreateIssueInput{
		Title: "after-checkpoint", Topic: "test", IssueType: "task", Prefix: "links",
	})
	if err != nil {
		t.Fatalf("seed post-checkpoint issue error = %v", err)
	}

	if _, err := withCommitLockTest(t, ctx, st, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, st.ResetToCheckpoint(ctx, cp.Name)
	}); err != nil {
		t.Fatalf("ResetToCheckpoint error = %v", err)
	}

	if _, err := st.GetIssue(ctx, pre.ID); err != nil {
		t.Fatalf("expected pre-checkpoint issue %s to survive reset, got error = %v", pre.ID, err)
	}
	if _, err := st.GetIssue(ctx, post.ID); err == nil {
		t.Fatalf("expected post-checkpoint issue %s to be gone after reset, but found it", post.ID)
	}
}

// TestCreateCheckpointRejectsInvalidPrefix proves the prefix guard rejects
// names that would produce ambiguous parses or unsafe Dolt branch names. We
// only check rejection here — the format constraints themselves are the
// regex's responsibility.
func TestCreateCheckpointRejectsInvalidPrefix(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	for _, bad := range []string{"", "UPPER", "with space", "with/slash", "1leading-digit"} {
		if _, err := st.CreateCheckpoint(ctx, bad, 3); err == nil {
			t.Fatalf("expected CreateCheckpoint(%q) to reject invalid prefix; got no error", bad)
		}
	}
}

// TestListCheckpointsIgnoresForeignBranches confirms that branches sharing a
// prefix substring but not the canonical "<prefix>-<unix-nanos>" shape are
// skipped. Otherwise a hand-created branch could slip into the checkpoint
// view and confuse retention math.
func TestListCheckpointsIgnoresForeignBranches(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if _, err := st.CreateCheckpoint(ctx, "shape", 5); err != nil {
		t.Fatalf("CreateCheckpoint error = %v", err)
	}
	if _, err := st.db.ExecContext(ctx, "CALL DOLT_BRANCH(?)", "shape-not-a-timestamp"); err != nil {
		t.Fatalf("seed foreign branch error = %v", err)
	}

	cps, err := st.ListCheckpoints(ctx, "shape")
	if err != nil {
		t.Fatalf("ListCheckpoints error = %v", err)
	}
	if len(cps) != 1 {
		t.Fatalf("expected 1 valid checkpoint, got %d (%v)", len(cps), checkpointNames(cps))
	}
}

func checkpointNames(cps []Checkpoint) []string {
	names := make([]string, 0, len(cps))
	for _, c := range cps {
		names = append(names, c.Name)
	}
	return names
}

// openTestStore opens a Store at a fresh temp directory and registers
// cleanup. Returns the Store ready for mutations.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), strings.ReplaceAll(t.Name(), "/", "_"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// withCommitLockTest wraps a checkpoint primitive call (which expects to run
// under the commit lock) in the Store's lock acquisition. The CLI / runner
// invocations run under the lock implicitly; tests that invoke primitives
// directly need this helper.
func withCommitLockTest[T any](t *testing.T, ctx context.Context, st *Store, fn func(context.Context) (T, error)) (T, error) {
	t.Helper()
	var result T
	err := st.withCommitLock(ctx, func(ctx context.Context) error {
		var inner error
		result, inner = fn(ctx)
		return inner
	})
	return result, err
}
