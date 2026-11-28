package store

import (
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// A restored backup runs on the config that was in force when it was taken:
// the applies are data the backup holds, so an apply made after it is not in
// force once the backup is restored.
func TestARestoredBackupRunsOnTheConfigInForceWhenItWasTaken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	saved, _, err := st.ApplyConfig(ctx, "[ready]\nrequired_fields = [\"prompt\"]\n", "ada")
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	backup, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, _, err := st.ApplyConfig(ctx, "[ready]\nrequired_fields = [\"description\"]\n", "ada"); err != nil {
		t.Fatalf("ApplyConfig after the backup: %v", err)
	}

	if _, err := st.ReplaceFromExport(ctx, backup); err != nil {
		t.Fatalf("ReplaceFromExport: %v", err)
	}
	applies, err := st.ConfigApplies(ctx)
	if err != nil {
		t.Fatalf("ConfigApplies: %v", err)
	}
	if inForce, applied := model.ConfigInForce(applies); len(applies) != 1 || !applied || inForce.ID != saved.ID {
		t.Errorf("applies after restore = %+v, want %s alone, in force", applies, saved.ID)
	}
}
