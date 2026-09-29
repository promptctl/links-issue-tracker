package cli

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func TestCensusOfCountsEveryIssueUnderTheRootItsIdHangsFrom(t *testing.T) {
	census := censusOf([]storage.IssueIdentity{
		{ID: "links-init-0q1s", Topic: "init"},
		// A child's own topic is not the one its id was rendered from; it is
		// counted through its root.
		{ID: "links-init-0q1s.kd1", Topic: "docs"},
		{ID: "links-rank-omey", Topic: "rank"},
		{ID: "demo-core-a1b2", Topic: "core"},
		{ID: "zed-core-c3d4", Topic: "core"},
		// Unreadable: a legacy shape, and a child whose root is gone.
		{ID: "links-42", Topic: "init"},
		{ID: "links-gone-zzzz.a1", Topic: "gone"},
	})
	// Descending by count, ties by prefix, unreadable last: every issue is
	// accounted for, so the total matches the store.
	if got, want := census.String(), "links:3,demo:1,zed:1,unreadable:2"; got != want {
		t.Fatalf("census = %q, want %q", got, want)
	}
	if census.mismatch("links") || census.mismatch("demo") {
		t.Fatalf("mismatch reported for a prefix the ids use")
	}
	if !census.mismatch("ab") {
		t.Fatalf("mismatch not reported for a prefix no id uses")
	}
}

func TestCensusOfAnEmptyStoreIsNoneAndNeverAMismatch(t *testing.T) {
	census := censusOf(nil)
	if got := census.String(); got != "none" {
		t.Fatalf("census = %q, want none", got)
	}
	// A fresh workspace has no ids to disagree with.
	if census.mismatch("anything") {
		t.Fatalf("mismatch reported over an empty store")
	}
}

func TestCensusSuggestsOnlyAPrefixSetWouldStoreAsIs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		issues []storage.IssueIdentity
		want   string
	}{
		{"the most-used prefix when it is legal", []storage.IssueIdentity{
			{ID: "links-core-a1b2", Topic: "core"}, {ID: "links-core-c3d4", Topic: "core"}, {ID: "demo-core-e5f6", Topic: "core"},
		}, "links"},
		// Minted when shorter prefixes were legal: adopting it would fail, so
		// the next legal one is named instead.
		{"the next legal prefix past an illegal most-used one", []storage.IssueIdentity{
			{ID: "ab-core-a1b2", Topic: "core"}, {ID: "ab-core-c3d4", Topic: "core"}, {ID: "demo-core-e5f6", Topic: "core"},
		}, "demo"},
		{"the placeholder when no prefix is legal", []storage.IssueIdentity{
			{ID: "ab-core-a1b2", Topic: "core"},
		}, "<prefix>"},
	} {
		if got := censusOf(tc.issues).adoptable(); got != tc.want {
			t.Errorf("%s: adoptable() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
