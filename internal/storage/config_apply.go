package storage

import (
	"context"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// ConfigStore holds the applies of the project config. An apply is never
// edited or removed: putting another config in force applies it.
type ConfigStore interface {
	// ApplyConfig puts content in force as applied by actor, and returns the
	// apply in force afterwards. When content is already the config in force
	// it writes nothing, and the bool is false.
	ApplyConfig(ctx context.Context, content string, actor string) (model.ConfigApply, bool, error)
	// ConfigApplies is every apply, in model.ConfigApplyOrdering.
	ConfigApplies(ctx context.Context) ([]model.ConfigApply, error)
}

// NextConfigApply is the apply that puts content in force over applies, or
// false when content is already in force. It is stamped by the engine's
// stamps against the newest apply, so an apply always takes force over every
// apply its writer saw. [LAW:single-enforcer] the one rule both engines apply
// by.
func NextConfigApply(applies []model.ConfigApply, id model.ID, content string, actor string, stamps *StampTally, clock Clock) (model.ConfigApply, bool) {
	inForce, applied := model.ConfigInForce(applies)
	at := stamps.Stamp(clock, inForce.CreatedAt)
	if applied && inForce.Content == content {
		return inForce, false
	}
	return model.ConfigApply{
		ID:        id,
		Content:   content,
		CreatedAt: at.Time(),
		CreatedBy: model.Text(actor),
	}, true
}
