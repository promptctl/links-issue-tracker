package storage

import (
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// ValidateRedirectTarget is the integrity floor under a redirecting close: the
// canonical it points at must be named, must not be the closing issue, and
// must not itself be deleted — a redirect into the trash is a dangling pointer
// by design. Archived stays legal, because "duplicate of something already
// done" is the most common real redirect. A non-redirecting transition carries
// no target, so this is nothing there.
//
// retentionOf reads the canonical's retention in the engine performing the
// write, and answers NotFoundError for an issue that is not there; it is the
// only part an engine supplies, so what makes a redirect legal is decided here
// and nowhere else. [LAW:single-enforcer]
func ValidateRedirectTarget(closing model.Issue, retentionOf func(model.ID) (model.Retention, error)) error {
	resolution := closing.ResolutionValue()
	target := closing.RedirectTargetValue()
	if target == nil {
		if resolution != nil && resolution.RedirectsToCanonical() {
			return fmt.Errorf("closing as %s requires a canonical target issue to redirect to", *resolution)
		}
		return nil
	}
	if model.ID(*target) == closing.ID {
		return fmt.Errorf("cannot redirect %s to itself", closing.ID)
	}
	retention, err := retentionOf(model.ID(*target))
	if err != nil {
		return err
	}
	if _, gone := retention.(model.Deleted); gone {
		return fmt.Errorf("cannot redirect %s to %s: the canonical issue is deleted", closing.ID, *target)
	}
	return nil
}
