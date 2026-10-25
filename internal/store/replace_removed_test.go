package store

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// removedBy names every held ticket the landed export does not hold, where a
// ticket is its id and its birth: one the export holds under the same id but
// born at another instant is a different ticket the replace writes over it.
func TestRemovedByIsEveryHeldTicketTheLandedExportDoesNotHold(t *testing.T) {
	born := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	held := []heldTicket{
		{id: "kept", title: "Kept", born: born},
		{id: "gone", title: "Gone", born: born},
		{id: "reborn", title: "Reborn", born: born},
	}
	landed := model.Export{Issues: []model.Issue{
		{ID: "kept", CreatedAt: born.In(time.FixedZone("elsewhere", 3600))},
		{ID: "reborn", CreatedAt: born.Add(time.Second)},
		{ID: "new", CreatedAt: born},
	}}
	removed := removedBy(held, landed)
	if len(removed) != 2 || removed[0].IssueID != "gone" || removed[0].Title != "Gone" || removed[1].IssueID != "reborn" {
		t.Errorf("removedBy = %+v, want gone and reborn: kept is one instant in another offset, new was never held", removed)
	}
}
