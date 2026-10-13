package merge

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// TestThreeWayAPurgeOutranksAConcurrentEdit pins why a purge is a record and
// not just a missing row. Without the record, "removed on one side, edited on
// the other" keeps the edit, so a peer that touched the ticket before it heard
// of the purge would hand it back. With it, the purged id is gone from the
// merge whichever side purged it, whatever the other side did to it, and with
// or without a common ancestor — and the record itself survives the merge, so
// it travels on to the next store.
func TestThreeWayAPurgeOutranksAConcurrentEdit(t *testing.T) {
	now := time.Now().UTC()
	mk := func(id, title string) model.Issue {
		return issueWithStatus(t, model.Issue{ID: id, Title: title, IssueType: "task", CreatedAt: now, UpdatedAt: now}, model.StateOpen)
	}
	purge := model.Purge{IssueID: "doomed", IssueCreatedAt: now, PurgedAt: now, PurgedBy: "grace"}
	held := model.Export{
		Issues:    []model.Issue{mk("doomed", "secret"), mk("kept", "kept")},
		Relations: []model.Relation{{SrcID: "kept", DstID: "doomed", Type: model.RelBlockedBy, CreatedAt: now}},
		Labels:    []model.Label{{IssueID: "doomed", Name: "leak", CreatedAt: now}},
	}
	edited := held
	edited.Issues = []model.Issue{mk("doomed", "secret, edited"), mk("kept", "kept, edited")}
	edited.Comments = []model.Comment{{ID: "c1", IssueID: "doomed", Body: "more secret", CreatedAt: now}}
	purged := model.Export{Issues: []model.Issue{mk("kept", "kept")}, Purges: []model.Purge{purge}}

	for _, tc := range []struct {
		name                string
		base, local, remote model.Export
	}{
		{"local purged, remote edited", held, purged, edited},
		{"remote purged, local edited", held, edited, purged},
		{"no common ancestor", model.Export{}, edited, purged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged := provisional(t, ThreeWay(tc.base, tc.local, tc.remote))
			if len(merged.Issues) != 1 || merged.Issues[0].ID != "kept" || merged.Issues[0].Title != "kept, edited" {
				t.Errorf("merged issues = %+v, want only kept, with its edit", merged.Issues)
			}
			if len(merged.Relations)+len(merged.Comments)+len(merged.Labels) != 0 {
				t.Errorf("merged kept rows of the purged issue: relations=%+v comments=%+v labels=%+v", merged.Relations, merged.Comments, merged.Labels)
			}
			if len(merged.Purges) != 1 || merged.Purges[0] != purge {
				t.Errorf("merged purges = %+v, want the record carried on", merged.Purges)
			}
		})
	}
}

// TestThreeWayKeepsAnotherTicketUnderAPurgedID pins that a purge removes the
// ticket it names, not every ticket that wears its id. In an unrelated-history
// combine, a side that never held the purged ticket can hold a different one
// under the same id; it is born at another instant, so no record names it, and
// it lands in the merge whole, beside the record.
func TestThreeWayKeepsAnotherTicketUnderAPurgedID(t *testing.T) {
	born := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	stranger := issueWithStatus(t, model.Issue{ID: "x", Title: "unrelated", IssueType: "task", CreatedAt: born.Add(time.Minute), UpdatedAt: born}, model.StateOpen)
	purge := model.Purge{IssueID: "x", IssueCreatedAt: born, PurgedAt: born, PurgedBy: "grace"}
	purged := model.Export{Purges: []model.Purge{purge}}
	holding := model.Export{
		Issues:   []model.Issue{stranger},
		Comments: []model.Comment{{ID: "c1", IssueID: "x", Body: "its own", CreatedAt: born}},
	}

	merged := provisional(t, ThreeWay(model.Export{}, purged, holding))
	if len(merged.Issues) != 1 || merged.Issues[0].Title != "unrelated" || len(merged.Comments) != 1 {
		t.Errorf("merged issues = %+v comments = %+v, want the unrelated ticket and its comment kept", merged.Issues, merged.Comments)
	}
	if len(merged.Purges) != 1 || merged.Purges[0] != purge {
		t.Errorf("merged purges = %+v, want the record carried on", merged.Purges)
	}
}
