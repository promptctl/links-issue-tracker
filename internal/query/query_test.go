package query

import (
	"reflect"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

func boolPtr(b bool) *bool { return &b }

// TestQueryTokenSupersetOfDiscreteFlags is the kkew.2 acceptance: for every
// filtering or list-shaping dimension `lit ls` exposes as a discrete flag, the
// --query token form must produce the identical store.ListIssuesFilter the flag
// produces. Each want filter below is exactly what runList assembles from the
// named flag; the token column is the superset claim. The four new dimensions —
// archived, deleted, sort, limit — sit alongside the ones that already had
// parity so a future token drop is caught here, not in the field.
func TestQueryTokenSupersetOfDiscreteFlags(t *testing.T) {
	updatedTS, err := time.Parse(time.RFC3339, "2026-03-07T10:00:00Z")
	if err != nil {
		t.Fatalf("time.Parse setup error = %v", err)
	}
	updatedUTC := updatedTS.UTC()

	cases := []struct {
		name  string
		flag  string // the discrete flag this token mirrors, for the failure message
		query string
		want  store.ListIssuesFilter
	}{
		{"status", "--status open", "status:open", store.ListIssuesFilter{Statuses: []model.State{model.StateOpen}}},
		{"type", "--type task", "type:task", store.ListIssuesFilter{IssueTypes: []model.IssueType{model.TypeTask}}},
		{"assignee", "--assignee bmf", "assignee:bmf", store.ListIssuesFilter{Assignees: []string{"bmf"}}},
		{"search", "--search login", "login", store.ListIssuesFilter{SearchTerms: []string{"login"}}},
		{"ids", "--ids issue-123", "id:issue-123", store.ListIssuesFilter{IDs: []string{"issue-123"}}},
		{"labels", "--labels renderer", "label:renderer", store.ListIssuesFilter{LabelsAll: []string{"renderer"}}},
		{"has-comments", "--has-comments", "has:comments", store.ListIssuesFilter{HasComments: boolPtr(true)}},
		{"updated-after", "--updated-after 2026-03-07T10:00:00Z", "updated>=2026-03-07T10:00:00Z", store.ListIssuesFilter{UpdatedAfter: &updatedUTC}},
		{"updated-before", "--updated-before 2026-03-07T10:00:00Z", "updated<=2026-03-07T10:00:00Z", store.ListIssuesFilter{UpdatedBefore: &updatedUTC}},
		{"archived", "--include-archived", "archived", store.ListIssuesFilter{IncludeArchived: true}},
		{"deleted", "--include-deleted", "deleted", store.ListIssuesFilter{IncludeDeleted: true}},
		{"sort", "--sort rank:asc", "sort:rank:asc", store.ListIssuesFilter{SortBy: []store.SortSpec{{Field: "rank", Desc: false}}}},
		{"limit", "--limit 5", "limit:5", store.ListIssuesFilter{Limit: 5}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tc.query, err)
			}
			// Merge onto an empty base is the no-flags path: the query is the sole
			// source, so the result must equal the flag-built filter for %s.
			got, err := Merge(store.ListIssuesFilter{}, parsed.Filter)
			if err != nil {
				t.Fatalf("Merge(empty, Parse(%q)) error = %v", tc.query, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("--query %q produced %#v, want the %q form %#v", tc.query, got, tc.flag, tc.want)
			}
		})
	}
}

// TestQueryMultiTokenAppliesAllFourNewTokens exercises the acceptance's concrete
// command: one query string carrying sort, limit, archived, and deleted together.
func TestQueryMultiTokenAppliesAllFourNewTokens(t *testing.T) {
	parsed, err := Parse(`sort:rank:asc limit:5 archived deleted`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	got, err := Merge(store.ListIssuesFilter{}, parsed.Filter)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	want := store.ListIssuesFilter{
		SortBy:          []store.SortSpec{{Field: "rank", Desc: false}},
		Limit:           5,
		IncludeArchived: true,
		IncludeDeleted:  true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("multi-token query produced %#v, want %#v", got, want)
	}
}

// TestQueryLimitRejectsNonInteger pins that a garbage limit is a loud error, not
// a silent fall-through to the uncapped default. [LAW:no-silent-failure]
func TestQueryLimitRejectsNonInteger(t *testing.T) {
	if _, err := Parse(`limit:lots`); err == nil {
		t.Fatal("expected error for non-integer limit")
	}
}

// TestQuerySortRejectsBadDirection pins that the sort: token reuses the store
// parser's rejection of an unknown direction rather than defaulting silently.
func TestQuerySortRejectsBadDirection(t *testing.T) {
	if _, err := Parse(`sort:rank:sideways`); err == nil {
		t.Fatal("expected error for unsupported sort direction")
	}
}

func TestParseBuildsFilterFromQueryExpression(t *testing.T) {
	result, err := Parse(`status:in_progress type:task assignee:bmf has:comments updated>=2026-03-07T10:00:00Z "render contract" id:issue-123 label:renderer`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(result.Filter.Statuses) != 1 || result.Filter.Statuses[0] != "in_progress" {
		t.Fatalf("Statuses = %q", result.Filter.Statuses)
	}
	if len(result.Filter.IssueTypes) != 1 || result.Filter.IssueTypes[0] != "task" {
		t.Fatalf("IssueTypes = %q", result.Filter.IssueTypes)
	}
	if len(result.Filter.Assignees) != 1 || result.Filter.Assignees[0] != "bmf" {
		t.Fatalf("Assignees = %q", result.Filter.Assignees)
	}
	if result.Filter.HasComments == nil || !*result.Filter.HasComments {
		t.Fatalf("HasComments = %#v", result.Filter.HasComments)
	}
	if result.Filter.UpdatedAfter == nil {
		t.Fatal("UpdatedAfter is nil")
	}
	if len(result.Filter.SearchTerms) != 1 || result.Filter.SearchTerms[0] != "render contract" {
		t.Fatalf("SearchTerms = %#v", result.Filter.SearchTerms)
	}
	if len(result.Filter.IDs) != 1 || result.Filter.IDs[0] != "issue-123" {
		t.Fatalf("IDs = %#v", result.Filter.IDs)
	}
	if len(result.Filter.LabelsAll) != 1 || result.Filter.LabelsAll[0] != "renderer" {
		t.Fatalf("LabelsAll = %#v", result.Filter.LabelsAll)
	}
}

func TestMergeMultipleStatusesCombines(t *testing.T) {
	base, err := Parse(`status:open`)
	if err != nil {
		t.Fatalf("Parse(base) error = %v", err)
	}
	incoming, err := Parse(`status:closed`)
	if err != nil {
		t.Fatalf("Parse(incoming) error = %v", err)
	}
	merged, err := Merge(base.Filter, incoming.Filter)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if len(merged.Statuses) != 2 {
		t.Fatalf("Statuses = %q, want [open closed]", merged.Statuses)
	}
}

func TestStatusAliasInProgressNormalizesToBeadsValue(t *testing.T) {
	result, err := Parse(`status:in-progress`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(result.Filter.Statuses) != 1 || result.Filter.Statuses[0] != "in_progress" {
		t.Fatalf("Statuses = %q, want [in_progress]", result.Filter.Statuses)
	}
}

func TestParseRejectsInvalidStatus(t *testing.T) {
	if _, err := Parse(`status:todo`); err == nil {
		t.Fatal("expected invalid status error")
	}
}

// The type: term parses through the sealed gate: a typo'd type is an error at
// the grammar seam, never an empty result, and an empty value is no longer a
// silent no-op. [LAW:no-silent-failure]
func TestParseRejectsInvalidType(t *testing.T) {
	if _, err := Parse(`type:bogus`); err == nil {
		t.Fatal("expected invalid type error")
	}
	if _, err := Parse(`type:`); err == nil {
		t.Fatal("expected invalid type error for empty value")
	}
}
