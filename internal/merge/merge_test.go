package merge

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model"
)

func issueWithStatus(t *testing.T, issue model.Issue, status model.State) model.Issue {
	t.Helper()
	hydrated, err := model.HydrateOwnedStatus(issue, model.StatusView{Value: status})
	if err != nil {
		t.Fatalf("HydrateOwnedStatus() error = %v", err)
	}
	return hydrated
}

func jsonRoundTripIssue(t *testing.T, issue model.Issue) model.Issue {
	t.Helper()
	data, err := json.Marshal(issue)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded model.Issue
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return decoded
}

func TestThreeWayDetectsPerIssueConflict(t *testing.T) {
	base := model.Export{Issues: []model.Issue{issueWithStatus(t, model.Issue{ID: "i1", Title: "issue", Priority: 0, IssueType: "task", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}, model.StateOpen)}}
	local := model.Export{Issues: append([]model.Issue(nil), base.Issues...)}
	remote := model.Export{Issues: append([]model.Issue(nil), base.Issues...)}
	local.Issues[0].Title = "local-change"
	remote.Issues[0].Title = "remote-change"

	result := ThreeWay(base, local, remote)
	if len(result.Conflicts) != 1 {
		t.Fatalf("conflicts = %#v", result.Conflicts)
	}
	if result.Conflicts[0].IssueID != "i1" {
		t.Fatalf("issue id = %q", result.Conflicts[0].IssueID)
	}
}

func TestThreeWayComparesJSONUnmarshaledEpicData(t *testing.T) {
	now := time.Now().UTC()
	hydratedEpic, err := model.HydrateAllOf(model.Issue{
		ID:        "epic-1",
		Title:     "base",
		IssueType: "epic",
		CreatedAt: now,
		UpdatedAt: now,
	}, nil)
	if err != nil {
		t.Fatalf("HydrateAllOf() error = %v", err)
	}
	baseIssue := jsonRoundTripIssue(t, hydratedEpic)
	base := model.Export{Issues: []model.Issue{baseIssue}}
	local := model.Export{Issues: []model.Issue{baseIssue}}
	remote := model.Export{Issues: []model.Issue{baseIssue}}
	remote.Issues[0].Title = "remote"

	result := ThreeWay(base, local, remote)
	if len(result.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts = %#v", result.Conflicts)
	}
	if len(result.Export.Issues) != 1 || result.Export.Issues[0].Title != "remote" {
		t.Fatalf("merged issues = %#v, want remote title", result.Export.Issues)
	}
}

func TestIssueEqualTreatsNilAndEmptyLabelsAsEquivalent(t *testing.T) {
	now := time.Now().UTC()
	base := issueWithStatus(t, model.Issue{ID: "i1", Title: "label test", Priority: 0, IssueType: "task", CreatedAt: now, UpdatedAt: now}, model.StateOpen)
	withNil := base
	withNil.Labels = nil
	withEmpty := base
	withEmpty.Labels = []string{}
	if !issueEqual(&withNil, &withEmpty) {
		t.Fatalf("issueEqual(nil, []) = false; nil and empty Labels must compare equal so JSON wire round-trip drift does not synthesize spurious changes")
	}
}

func TestThreeWayMergesNonConflictingIssueChanges(t *testing.T) {
	now := time.Now().UTC()
	base := model.Export{
		WorkspaceID: "ws",
		Issues: []model.Issue{
			issueWithStatus(t, model.Issue{ID: "i1", Title: "one", Priority: 0, IssueType: "task", CreatedAt: now, UpdatedAt: now}, model.StateOpen),
			issueWithStatus(t, model.Issue{ID: "i2", Title: "two", Priority: 0, IssueType: "task", CreatedAt: now, UpdatedAt: now}, model.StateOpen),
		},
	}
	local := model.Export{WorkspaceID: base.WorkspaceID, Issues: append([]model.Issue(nil), base.Issues...)}
	local.Issues[0].Title = "local i1"
	remote := model.Export{WorkspaceID: base.WorkspaceID, Issues: append([]model.Issue(nil), base.Issues...)}
	remote.Issues[1].Title = "remote i2"

	result := ThreeWay(base, local, remote)
	if len(result.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts = %#v", result.Conflicts)
	}
	if len(result.Export.Issues) != 2 {
		t.Fatalf("issues = %#v", result.Export.Issues)
	}
	merged := map[string]string{}
	for _, issue := range result.Export.Issues {
		merged[issue.ID] = issue.Title
	}
	if merged["i1"] != "local i1" || merged["i2"] != "remote i2" {
		t.Fatalf("merged titles = %#v", merged)
	}
}
