package merge

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func issueWithStatus(t *testing.T, issue model.Issue, status model.State) model.Issue {
	t.Helper()
	hydrated, err := model.HydrateStatus(issue, model.StatusView{Value: status})
	if err != nil {
		t.Fatalf("HydrateStatus() error = %v", err)
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

func TestThreeWayEmitsProsePendingForConcurrentTitleRewrite(t *testing.T) {
	base := model.Export{Issues: []model.Issue{issueWithStatus(t, model.Issue{ID: "i1", Title: "issue", Priority: 0, IssueType: "task", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}, model.StateOpen)}}
	local := model.Export{Issues: append([]model.Issue(nil), base.Issues...)}
	remote := model.Export{Issues: append([]model.Issue(nil), base.Issues...)}
	local.Issues[0].Title = "local-change"
	remote.Issues[0].Title = "remote-change"

	result := ThreeWay(base, local, remote)
	if len(result.Pending) != 1 {
		t.Fatalf("pending = %#v", result.Pending)
	}
	got := result.Pending[0]
	if got.IssueID != "i1" || got.Field != ProseTitle {
		t.Fatalf("pending ref = %#v", got)
	}
	if got.Base != "issue" || got.Ours != "local-change" || got.Theirs != "remote-change" {
		t.Fatalf("pending texts = %#v", got)
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
	if len(result.Pending) != 0 {
		t.Fatalf("unexpected pending = %#v", result.Pending)
	}
	if len(result.Provisional().Issues) != 1 || result.Provisional().Issues[0].Title != "remote" {
		t.Fatalf("merged issues = %#v, want remote title", result.Provisional().Issues)
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

// issueClosedWith builds a closed leaf that is byte-for-byte identical across
// calls except for its resolution — fixed timestamps keep the resolution the
// ONLY varying field, so a detected change can only be the resolution divergence.
func issueClosedWith(t *testing.T, id string, closedAt time.Time, resolution model.Resolution) model.Issue {
	t.Helper()
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	res := resolution
	hydrated, err := model.HydrateStatus(
		model.Issue{ID: id, Title: id, Priority: 0, IssueType: "task", CreatedAt: fixed, UpdatedAt: fixed},
		model.StatusView{Value: model.StateClosed, ClosedAt: &closedAt, Resolution: &res},
	)
	if err != nil {
		t.Fatalf("HydrateStatus() error = %v", err)
	}
	return hydrated
}

// A side that re-closes an already-closed ticket with a DIFFERENT resolution
// (status stays closed, same closed_at) has genuinely moved the row. The
// change-gate must detect it, or ThreeWay routes the side through the
// "unchanged" branch and the new resolution is silently dropped.
func TestIssueChangedDetectsResolutionOnlyDivergence(t *testing.T) {
	closedAt := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	base := issueClosedWith(t, "i1", closedAt, model.ResolutionWontfix)
	reResolved := issueClosedWith(t, "i1", closedAt, model.ResolutionDuplicate)
	if !issueChanged(&base, &reResolved) {
		t.Fatalf("issueChanged = false for a resolution-only re-close (wontfix -> duplicate); the edit would be dropped on sync")
	}
}

func TestThreeWayPreservesResolutionOnlyReClose(t *testing.T) {
	closedAt := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	baseIssue := issueClosedWith(t, "i1", closedAt, model.ResolutionWontfix)
	base := model.Export{WorkspaceID: "ws", Issues: []model.Issue{baseIssue}}
	// remote leaves the ticket exactly as base; local re-closes with a new reason.
	remote := model.Export{WorkspaceID: "ws", Issues: []model.Issue{baseIssue}}
	local := model.Export{WorkspaceID: "ws", Issues: []model.Issue{issueClosedWith(t, "i1", closedAt, model.ResolutionDuplicate)}}

	result := ThreeWay(base, local, remote)
	if len(result.Pending) != 0 {
		t.Fatalf("unexpected pending = %#v", result.Pending)
	}
	issues := result.Provisional().Issues
	if len(issues) != 1 {
		t.Fatalf("issues = %#v", issues)
	}
	got := issues[0].ResolutionValue()
	if got == nil || *got != model.ResolutionDuplicate {
		t.Fatalf("merged resolution = %v, want duplicate; the lone-side resolution edit must survive the merge", got)
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
	if len(result.Pending) != 0 {
		t.Fatalf("unexpected pending = %#v", result.Pending)
	}
	if len(result.Provisional().Issues) != 2 {
		t.Fatalf("issues = %#v", result.Provisional().Issues)
	}
	merged := map[string]string{}
	for _, issue := range result.Provisional().Issues {
		merged[issue.ID] = issue.Title
	}
	if merged["i1"] != "local i1" || merged["i2"] != "remote i2" {
		t.Fatalf("merged titles = %#v", merged)
	}
}
