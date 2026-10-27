package store

import (
	"context"
	"testing"
)

// TestDeleteBranchesUnderMatchesThePrefixLiterally deletes the branches under
// a prefix holding SQL LIKE's wildcard: the branch the prefix names goes, and
// one the wildcard alone would match stays.
func TestDeleteBranchesUnderMatchesThePrefixLiterally(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openSyncOrFatal(t, ctx, migratedDoltDir(t))
	defer func() { _ = st.Close() }()
	for _, name := range []string{"scratch_a/one", "scratchXa/keep"} {
		if err := execProcedureDiscard(ctx, st.db, "DOLT_BRANCH", name); err != nil {
			t.Fatalf("create branch %s: %v", name, err)
		}
	}
	if err := st.runSyncMutation(ctx, func(ctx context.Context) error {
		return st.deleteBranchesUnder(ctx, "scratch_a/", "")
	}); err != nil {
		t.Fatalf("deleteBranchesUnder() error = %v", err)
	}
	if left := branchesUnder(t, ctx, st, "scratch"); len(left) != 1 || left["Xa/keep"] == "" {
		t.Fatalf("branches left under scratch = %v; want only scratchXa/keep", left)
	}
}
