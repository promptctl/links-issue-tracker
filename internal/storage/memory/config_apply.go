package memory

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// ApplyConfig puts content in force by storage.NextConfigApply's rule.
func (e *Engine) ApplyConfig(ctx context.Context, content string, actor string) (model.ConfigApply, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	apply, wrote := storage.NextConfigApply(e.configApplies(), model.ID("cfg-"+uuid.NewString()), content, actor, &e.stamps, e.clock)
	if wrote {
		e.applies = append(e.applies, apply)
	}
	return apply, wrote, nil
}

// ConfigApplies is every apply, in model.ConfigApplyOrdering.
func (e *Engine) ConfigApplies(ctx context.Context) ([]model.ConfigApply, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.configApplies(), nil
}

// configApplies copies the applies for a caller to keep, in
// model.ConfigApplyOrdering.
func (e *Engine) configApplies() []model.ConfigApply {
	out := slices.Clone(e.applies)
	slices.SortFunc(out, model.ConfigApplyOrdering)
	return out
}
