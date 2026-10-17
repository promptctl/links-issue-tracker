package storage

import (
	"slices"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The conformance suite asserts the trailing id key through each engine, but
// it inherits the order rows reach the fold in, and the Dolt engine hands them
// back in primary-key order — id order already — so there the rule's absence
// would pass. Here the rows arrive in the reverse of the answer and a stable
// sort keeps whatever order it is handed, so the tie-break is the only thing
// that can produce it.
func TestIssueOrderingBreaksTiesByIDWhateverOrderTheRowsArriveIn(t *testing.T) {
	handed := []model.Issue{{ID: "c", Title: "same"}, {ID: "b", Title: "same"}, {ID: "a", Title: "same"}}
	for _, desc := range []bool{false, true} {
		order, err := IssueOrdering([]SortSpec{{Field: "title", Desc: desc}}, RankAncestry{})
		if err != nil {
			t.Fatalf("IssueOrdering error = %v", err)
		}
		sorted := slices.Clone(handed)
		slices.SortStableFunc(sorted, order)
		got := make([]string, 0, len(sorted))
		for _, issue := range sorted {
			got = append(got, issue.ID)
		}
		// Descending reverses the named key, never the tie-break.
		if want := []string{"a", "b", "c"}; !slices.Equal(got, want) {
			t.Errorf("desc=%v: tied rows ordered %v, want %v", desc, got, want)
		}
	}
}
