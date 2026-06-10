package cli

import (
	"reflect"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
)

// TestClassifyReadinessPerKind is the contract test for the single
// annotation→readiness enforcer: every annotation kind maps to exactly one
// classification family (blocking, orphaned, rank hygiene) or to none
// (FocusPath — an ordering fact, deliberately invisible to readiness).
func TestClassifyReadinessPerKind(t *testing.T) {
	cases := []struct {
		name           string
		ann            annotation.Annotation
		wantReady      bool
		wantBlocking   []BlockingReason
		wantOrphaned   bool
		wantInversions []string
	}{
		{
			name:         "missing_field blocks",
			ann:          annotation.Annotation{Kind: annotation.MissingField, Message: "title"},
			wantReady:    false,
			wantBlocking: []BlockingReason{{Kind: annotation.MissingField, Detail: "title"}},
		},
		{
			name:         "open_dependency blocks",
			ann:          annotation.Annotation{Kind: annotation.OpenDependency, Message: "dep-1"},
			wantReady:    false,
			wantBlocking: []BlockingReason{{Kind: annotation.OpenDependency, Detail: "dep-1"}},
		},
		{
			name:         "needs_design blocks",
			ann:          annotation.Annotation{Kind: annotation.NeedsDesign, Message: NeedsDesignLabel},
			wantReady:    false,
			wantBlocking: []BlockingReason{{Kind: annotation.NeedsDesign, Detail: NeedsDesignLabel}},
		},
		{
			name:         "earlier_sibling_pending blocks",
			ann:          annotation.Annotation{Kind: annotation.EarlierSiblingPending, Message: "sib-1"},
			wantReady:    false,
			wantBlocking: []BlockingReason{{Kind: annotation.EarlierSiblingPending, Detail: "sib-1"}},
		},
		{
			name:         "orphaned is staleness, not blocking",
			ann:          annotation.Annotation{Kind: annotation.Orphaned, Message: "in_progress for 7h"},
			wantReady:    true,
			wantOrphaned: true,
		},
		{
			name:           "rank_inversion is hygiene, not blocking",
			ann:            annotation.Annotation{Kind: annotation.RankInversion, Message: "dep-1"},
			wantReady:      true,
			wantInversions: []string{"dep-1"},
		},
		{
			name:      "focus_path is ordering, invisible to readiness",
			ann:       annotation.Annotation{Kind: annotation.FocusPath, Message: "goal-1"},
			wantReady: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ClassifyReadiness([]annotation.Annotation{tc.ann})
			if got := r.IsReady(); got != tc.wantReady {
				t.Errorf("IsReady() = %v, want %v", got, tc.wantReady)
			}
			if got := r.BlockingReasons(); !reflect.DeepEqual(got, tc.wantBlocking) {
				t.Errorf("BlockingReasons() = %v, want %v", got, tc.wantBlocking)
			}
			if got := r.IsOrphaned(); got != tc.wantOrphaned {
				t.Errorf("IsOrphaned() = %v, want %v", got, tc.wantOrphaned)
			}
			if got := r.RankInversions(); !reflect.DeepEqual(got, tc.wantInversions) {
				t.Errorf("RankInversions() = %v, want %v", got, tc.wantInversions)
			}
		})
	}
}

// TestClassifyReadinessNoAnnotations pins the project invariant: an empty
// annotation set classifies as ready because there are zero blocking reasons —
// readiness is always the typed interpretation, never `len(annotations) == 0`
// used as a proxy.
func TestClassifyReadinessNoAnnotations(t *testing.T) {
	r := ClassifyReadiness(nil)
	if !r.IsReady() {
		t.Fatal("no annotations must classify as ready")
	}
	if r.IsOrphaned() || len(r.RankInversions()) != 0 || len(r.DependencyIDs()) != 0 {
		t.Fatal("no annotations must classify with zero facts in every family")
	}
}

// TestClassifyReadinessComposite exercises one issue carrying facts from every
// family at once: blocking, staleness, hygiene, and ordering coexist without
// masking each other, and DependencyIDs projects only the open dependencies.
func TestClassifyReadinessComposite(t *testing.T) {
	r := ClassifyReadiness([]annotation.Annotation{
		{Kind: annotation.OpenDependency, Message: "dep-1"},
		{Kind: annotation.OpenDependency, Message: "dep-2"},
		{Kind: annotation.MissingField, Message: "title"},
		{Kind: annotation.RankInversion, Message: "dep-2"},
		{Kind: annotation.Orphaned, Message: "in_progress for 7h"},
		{Kind: annotation.FocusPath, Message: "goal-1"},
	})
	if r.IsReady() {
		t.Error("blocking reasons present, must not be ready")
	}
	if got := len(r.BlockingReasons()); got != 3 {
		t.Errorf("BlockingReasons() len = %d, want 3", got)
	}
	if got, want := r.DependencyIDs(), []string{"dep-1", "dep-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DependencyIDs() = %v, want %v", got, want)
	}
	if !r.IsOrphaned() {
		t.Error("orphaned fact lost in composite classification")
	}
	if got, want := r.RankInversions(), []string{"dep-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("RankInversions() = %v, want %v", got, want)
	}
}

// TestBlockingKindCounts pins the blocked-summary aggregation contract: counts
// are per issue per kind (several reasons of one kind on one issue count
// once), emitted in the canonical blocking-kind order, zero-count kinds
// omitted.
func TestBlockingKindCounts(t *testing.T) {
	rs := []IssueReadiness{
		ClassifyReadiness([]annotation.Annotation{
			{Kind: annotation.OpenDependency, Message: "dep-1"},
			{Kind: annotation.OpenDependency, Message: "dep-2"},
			{Kind: annotation.MissingField, Message: "title"},
		}),
		ClassifyReadiness([]annotation.Annotation{
			{Kind: annotation.OpenDependency, Message: "dep-3"},
		}),
	}
	got := blockingKindCounts(rs)
	want := []kindCount{
		{Kind: annotation.MissingField, Count: 1},
		{Kind: annotation.OpenDependency, Count: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blockingKindCounts() = %v, want %v", got, want)
	}
}
