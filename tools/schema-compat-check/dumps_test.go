package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/release"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

func table(name string, cols []string, rows ...[]any) store.RawTable {
	if rows == nil {
		rows = [][]any{}
	}
	return store.RawTable{Name: name, Columns: cols, Rows: rows}
}

func dump(tables ...store.RawTable) store.RawDump { return store.RawDump{Tables: tables} }

var (
	knownIssues = table("issues", []string{"id", "title", "updated_at"}, []any{"t-1", "one", "d1"})
	edit        = titleEdit{issueID: "t-1", title: "edited"}
)

func TestUnexercisedNamesNewSchemaTheFixtureLeftEmpty(t *testing.T) {
	known := dump(knownIssues)
	tree := dump(
		table("issues", []string{"id", "title", "updated_at", "work_plan"}, []any{"t-1", "one", "d1", ""}),
		table("plan_reviews", []string{"id"}),
	)
	got := unexercised(known, tree)
	want := []string{"column issues.work_plan holds no data", "table plan_reviews has no rows"}
	if !slices.Equal(got, want) {
		t.Fatalf("unexercised = %q, want %q", got, want)
	}
}

func TestUnexercisedIsEmptyWhenTheFixtureWroteEveryNewTableAndColumn(t *testing.T) {
	known := dump(knownIssues)
	tree := dump(
		table("issues", []string{"id", "title", "updated_at", "work_plan"}, []any{"t-1", "one", "d1", "a plan"}),
		table("plan_reviews", []string{"id"}, []any{"r-1"}),
	)
	if got := unexercised(known, tree); len(got) != 0 {
		t.Fatalf("unexercised = %q, want none", got)
	}
}

func TestLostAllowsTheTitleEditAlone(t *testing.T) {
	before := dump(table("issues", []string{"id", "title", "updated_at", "work_plan"}, []any{"t-1", "one", "d1", "a plan"}))
	after := dump(table("issues", []string{"id", "title", "updated_at", "work_plan"}, []any{"t-1", "edited", "d2", "a plan"}))
	if got := lost(before, after, edit); len(got) != 0 {
		t.Fatalf("lost = %q, want none", got)
	}
}

func TestLostNamesAColumnTheRoundTripReset(t *testing.T) {
	before := dump(table("issues", []string{"id", "title", "updated_at", "work_plan"}, []any{"t-1", "one", "d1", "a plan"}))
	after := dump(table("issues", []string{"id", "title", "updated_at", "work_plan"}, []any{"t-1", "edited", "d2", ""}))
	got := lost(before, after, edit)
	if len(got) != 1 || !strings.Contains(got[0], "column work_plan was a plan") {
		t.Fatalf("lost = %q, want the reset work_plan", got)
	}
}

func TestLostNamesARowTheRoundTripDropped(t *testing.T) {
	reviews := []string{"id", "issue_id"}
	before := dump(knownIssues, table("plan_reviews", reviews, []any{"r-1", "t-1"}))
	after := dump(table("issues", knownIssues.Columns, []any{"t-1", "edited", "d2"}), table("plan_reviews", reviews))
	got := lost(before, after, edit)
	if len(got) != 1 || !strings.Contains(got[0], "table plan_reviews lost or changed row") {
		t.Fatalf("lost = %q, want the dropped plan review", got)
	}
}

func TestLostNamesATitleTheEditDidNotLand(t *testing.T) {
	after := dump(table("issues", knownIssues.Columns, []any{"t-1", "one", "d1"}))
	got := lost(dump(knownIssues), after, edit)
	if len(got) != 1 || !strings.Contains(got[0], `want the release's edit "edited"`) {
		t.Fatalf("lost = %q, want the missing edit", got)
	}
}

func mustTag(t *testing.T, s string) release.Tag {
	t.Helper()
	tag, err := release.ParseTag(s)
	if err != nil {
		t.Fatal(err)
	}
	return tag
}

func tags(ps []published) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.tag.String()
	}
	return out
}

// The release list as published, when v0.20.1 was the newest release: 30 days
// reach back past v0.15.0, so the window is every release from v0.15.0 on.
func TestWindowTakesEveryReleaseOfTheLast30Days(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	releases := []published{
		{mustTag(t, "v0.20.1"), at("2026-10-17T19:48:17Z")},
		{mustTag(t, "v0.20.0"), at("2026-10-17T14:59:51Z")},
		{mustTag(t, "v0.19.0"), at("2026-10-17T00:42:33Z")},
		{mustTag(t, "v0.18.0"), at("2026-10-15T18:15:10Z")},
		{mustTag(t, "v0.17.0"), at("2026-10-15T07:36:16Z")},
		{mustTag(t, "v0.16.0"), at("2026-09-28T06:31:25Z")},
		{mustTag(t, "v0.15.0"), at("2026-09-27T05:03:02Z")},
		{mustTag(t, "v0.14.0"), at("2026-09-10T00:00:00Z")},
	}
	got := tags(window(releases, at("2026-10-18T00:00:00Z")))
	want := []string{"v0.20.1", "v0.20.0", "v0.19.0", "v0.18.0", "v0.17.0", "v0.16.0", "v0.15.0"}
	if !slices.Equal(got, want) {
		t.Fatalf("window = %v, want %v", got, want)
	}
}

func TestWindowKeepsTheNewestTwoHoweverOld(t *testing.T) {
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	releases := []published{
		{mustTag(t, "v0.1.0"), old},
		{mustTag(t, "v0.3.0"), old.Add(48 * time.Hour)},
		{mustTag(t, "v0.2.0"), old.Add(24 * time.Hour)},
	}
	got := tags(window(releases, old.AddDate(1, 0, 0)))
	if want := []string{"v0.3.0", "v0.2.0"}; !slices.Equal(got, want) {
		t.Fatalf("window = %v, want %v", got, want)
	}
}
