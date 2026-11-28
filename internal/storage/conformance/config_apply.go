package conformance

import (
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

const (
	requirePrompt = "[ready]\nrequired_fields = [\"prompt\"]\n"
	soilMode      = "[quickstart]\nsoil_mode = true\n"
)

// aConfigApplyPutsItsContentInForce holds an apply to the rule a reader of the
// store relies on: the newest apply is the config in force, an apply of the
// config already in force writes nothing, and the export carries every apply.
func aConfigApplyPutsItsContentInForce(t *testing.T, ctx context.Context, st storage.Store, _ *clock) {
	first, wrote, err := st.ApplyConfig(ctx, requirePrompt, "ada")
	if err != nil || !wrote {
		t.Fatalf("first apply = (%+v, %v, %v), want it written", first, wrote, err)
	}
	if first.Content != requirePrompt || first.CreatedBy != "ada" {
		t.Errorf("first apply = %+v, want ada's apply of %q", first, requirePrompt)
	}
	again, wrote, err := st.ApplyConfig(ctx, requirePrompt, "grace")
	if err != nil || wrote || again.ID != first.ID {
		t.Fatalf("re-apply of the config in force = (%+v, %v, %v), want the apply in force back and nothing written", again, wrote, err)
	}
	second, wrote, err := st.ApplyConfig(ctx, soilMode, "grace")
	if err != nil || !wrote {
		t.Fatalf("second apply = (%+v, %v, %v), want it written", second, wrote, err)
	}
	applies, err := st.ConfigApplies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(applies) != 2 || applies[0].ID != first.ID || applies[1].ID != second.ID {
		t.Fatalf("applies = %+v, want the first then the second, oldest first", applies)
	}
	if inForce, _ := model.ConfigInForce(applies); inForce.ID != second.ID {
		t.Errorf("in force = %+v, want the second apply", inForce)
	}
	export := mustExport(t, ctx, st)
	if len(export.ConfigApplies) != 2 || export.ConfigApplies[0] != applies[0] || export.ConfigApplies[1] != applies[1] {
		t.Errorf("export applies = %+v, want %+v", export.ConfigApplies, applies)
	}
}

// aConfigApplyOutranksEveryApplyItSaw is an apply made on a clock that runs
// behind the apply in force: it is stamped after that apply, so it takes force
// over it rather than sorting beneath it.
func aConfigApplyOutranksEveryApplyItSaw(t *testing.T, ctx context.Context, st storage.Store, clk *clock) {
	clk.Pin(laterInstant)
	first, _, err := st.ApplyConfig(ctx, requirePrompt, "ada")
	if err != nil {
		t.Fatal(err)
	}
	clk.Pin(earlierInstant)
	second, _, err := st.ApplyConfig(ctx, soilMode, "grace")
	if err != nil {
		t.Fatal(err)
	}
	if !second.CreatedAt.After(first.CreatedAt) {
		t.Errorf("an apply on a clock behind the one in force is stamped %v, want after %v", second.CreatedAt, first.CreatedAt)
	}
	applies, err := st.ConfigApplies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if inForce, _ := model.ConfigInForce(applies); inForce.ID != second.ID {
		t.Errorf("in force = %+v, want the apply made last", inForce)
	}
}
