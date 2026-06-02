package store

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// rankedDump is a clean-shape dump carrying an item_rank column with distinct,
// well-formed lexorank values. It is the fixture for the rank-permutation law:
// a faithful rebuild conserves the order, and the priority->rank mis-map breaks
// it. DeterministicMap recognizes every column (item_rank is the v1 rank name),
// so it stands in for any producer's valid mapping.
func rankedDump() RawDump {
	const created = "2026-01-01T00:00:00Z"
	const closed = "2026-01-02T00:00:00Z"
	return RawDump{WorkspaceID: "ranked-ws", Tables: []RawTable{
		{Name: "issues",
			Columns: []string{"id", "title", "description", "status", "priority", "issue_type", "created_at", "updated_at", "closed_at", "item_rank"},
			Rows: [][]any{
				{"i1", "First", "desc one", "open", int64(0), "task", created, created, nil, "V"},
				{"i2", "Second", "desc two", "closed", int64(0), "task", created, closed, closed, "h"},
			}},
	}}
}

// TestVerifyCandidateReconciledOnFaithfulRebuild is acceptance #1: a correct
// rebuild passes — Doctor-clean and every conservation law holds.
func TestVerifyCandidateReconciledOnFaithfulRebuild(t *testing.T) {
	ctx := context.Background()
	dump := rankedDump()

	cand, err := RebuildCandidate(ctx, t.TempDir(), dump, mustMap(t, dump))
	if err != nil {
		t.Fatalf("RebuildCandidate rejected a valid mapping: %v", err)
	}
	t.Cleanup(func() { _ = cand.Discard() })

	report, err := VerifyCandidate(ctx, dump, mustMap(t, dump), cand.Store())
	if err != nil {
		t.Fatalf("VerifyCandidate: %v", err)
	}
	if !report.Reconciled() {
		t.Fatalf("faithful rebuild rejected; findings:\n%s", report)
	}
}

// TestVerifyCandidateRejectsMisMappedRank is acceptance #2: a deliberately
// mis-mapped rebuild (priority<->rank swap) is REJECTED with a discrepancy report
// that localizes the mismatch. The swap is the realistic mis-map that survives
// Validate (both targets stay covered, no duplicate target, identity transforms)
// yet corrupts the data: priority values collide as ranks. The gate must catch it
// by the rank-permutation law, naming the colliding ids — not by guessing.
func TestVerifyCandidateRejectsMisMappedRank(t *testing.T) {
	ctx := context.Background()
	dump := rankedDump()

	swapped := swapTargets(mustMap(t, dump),
		ColumnRef{Table: "issues", Column: "priority"}, ColumnRef{Table: "issues", Column: "item_rank"})

	cand, err := RebuildCandidate(ctx, t.TempDir(), dump, swapped)
	if err != nil {
		t.Fatalf("RebuildCandidate should accept the mis-map (it is well-formed); the gate, not the applier, must reject it: %v", err)
	}
	t.Cleanup(func() { _ = cand.Discard() })

	report, err := VerifyCandidate(ctx, dump, swapped, cand.Store())
	if err != nil {
		t.Fatalf("VerifyCandidate: %v", err)
	}
	if report.Reconciled() {
		t.Fatal("gate accepted a mis-mapped rebuild; priority values landed in rank and must break the permutation law")
	}
	if !hasLaw(report, LawRank) {
		t.Fatalf("expected a rank-permutation finding localizing the mismatch; got:\n%s", report)
	}
	// The report must localize: name the colliding ids so it is a usable repair
	// prompt, not just "something is wrong".
	rendered := report.String()
	if !strings.Contains(rendered, "i1") || !strings.Contains(rendered, "i2") {
		t.Fatalf("rank finding did not localize the colliding issue ids; got:\n%s", rendered)
	}
}

// TestCountFindingsDetectsRowLoss exercises count conservation in isolation
// against the raw dump, with no round-trip: a collection short of its source row
// count is reported, naming the collection and both counts.
func TestCountFindingsDetectsRowLoss(t *testing.T) {
	dump := rankedDump() // 2 issue rows
	// A rebuild that lost one issue.
	export := model.Export{Issues: []model.Issue{{ID: "i1"}}}

	findings := countFindings(dump, mustMap(t, dump), export)
	if len(findings) != 1 || findings[0].Law != LawCount {
		t.Fatalf("expected one count finding for the lost issue; got %+v", findings)
	}
	if !strings.Contains(findings[0].Detail, "issues") ||
		!strings.Contains(findings[0].Detail, "2") || !strings.Contains(findings[0].Detail, "1") {
		t.Fatalf("count finding did not localize collection and counts: %q", findings[0].Detail)
	}
}

// TestCountFindingsExcludesConditionalFanOutChildren proves the non-circularity
// rule for fan-out: an Always emitter (issue_history → events) is count-conserved
// against the raw row count, while the CONDITIONAL child collection
// (event_changes, emitted only on a real status transition) is excluded — its
// record count is a function of the mapping itself, so comparing it to a
// mapping-derived expectation would be a tautology. A rebuild whose event count
// matches the row count must produce no count finding, no matter how many change
// rows it nests.
func TestCountFindingsExcludesConditionalFanOutChildren(t *testing.T) {
	const created = "2026-01-01T10:00:00Z"
	dump := RawDump{WorkspaceID: "w", Tables: []RawTable{
		{Name: "issue_history",
			Columns: legacyIssueHistoryColumns,
			Rows: [][]any{
				{"h1", "i1", "start", "began", "open", "in_progress", created, "alice"},
				{"h2", "i1", "close", "shipped", "in_progress", "closed", created, "alice"},
			}},
	}}
	m, ok := DeterministicMap(dump)
	if !ok {
		t.Fatal("DeterministicMap declined the canonical issue_history shape")
	}
	// Two events (matching the two source rows) but a deliberately odd number of
	// nested change rows: if event_changes were count-conserved, expected (0, since
	// no Always emitter feeds it) vs actual (3) would spuriously fire.
	export := model.Export{Events: []model.IssueEvent{
		{ID: "h1", Changes: []model.FieldChange{{Field: "status"}, {Field: "status"}}},
		{ID: "h2", Changes: []model.FieldChange{{Field: "status"}}},
	}}
	if findings := countFindings(dump, m, export); len(findings) != 0 {
		t.Fatalf("conditional fan-out child must be excluded from count conservation; got %+v", findings)
	}

	// The Always-emitted parent IS conserved: drop an event and the count law fires.
	lost := model.Export{Events: []model.IssueEvent{{ID: "h1"}}}
	findings := countFindings(dump, m, lost)
	if len(findings) != 1 || findings[0].Law != LawCount || !strings.Contains(findings[0].Detail, "events") {
		t.Fatalf("event count loss must be reported (events is an Always emitter); got %+v", findings)
	}
}

// TestIDStabilityFindingsDetectsLostAndExtraIDs exercises the id-stability law in
// isolation: an id present in the source but missing from the rebuild, and an id
// the rebuild invented, are each reported.
func TestIDStabilityFindingsDetectsLostAndExtraIDs(t *testing.T) {
	dump := rankedDump() // source issue ids: i1, i2
	export := model.Export{Issues: []model.Issue{{ID: "i1"}, {ID: "i9"}}}

	findings := idStabilityFindings(dump, mustMap(t, dump), export)
	if len(findings) != 2 {
		t.Fatalf("expected a missing-id and an extra-id finding; got %+v", findings)
	}
	joined := findings[0].Detail + " " + findings[1].Detail
	if !strings.Contains(joined, "i2") || !strings.Contains(joined, "i9") {
		t.Fatalf("id-stability findings did not name the lost (i2) and extra (i9) ids: %q", joined)
	}
	for _, f := range findings {
		if f.Law != LawIDStability {
			t.Fatalf("expected id_stability law, got %q", f.Law)
		}
	}
}

// TestSourceValuesForAggregatesAcrossTables guards the conserved-value set
// against the first-match bug: when more than one table maps a column onto the
// same target (legal — Validate's duplicate-target check is per-table, and count
// conservation sums across tables), the reference set is the UNION of every
// contributing column, so id stability does not flag a second table's ids as
// spuriously extra.
func TestSourceValuesForAggregatesAcrossTables(t *testing.T) {
	dump := RawDump{Tables: []RawTable{
		{Name: "issues", Columns: []string{"id"}, Rows: [][]any{{"i1"}, {"i2"}}},
		{Name: "issues_overflow", Columns: []string{"id"}, Rows: [][]any{{"i3"}}},
	}}
	idEmitter := func(table string) TableMapping {
		return TableMapping{Table: table, Emitters: []Emitter{{
			Collection: collIssues, When: Always{},
			Fields: map[string]FieldSource{"id": FromColumn{Column: "id", Transform: TransformIdentity}},
		}}}
	}
	m := ShapeMapping{Tables: []TableMapping{idEmitter("issues"), idEmitter("issues_overflow")}}

	values, ok := sourceValuesFor(dump, m, "issues.id")
	if !ok {
		t.Fatal("expected a column mapped to issues.id")
	}
	sort.Strings(values)
	if strings.Join(values, ",") != "i1,i2,i3" {
		t.Fatalf("sourceValuesFor did not aggregate across tables; got %v", values)
	}
}

// swapTargets returns a copy of m in which the two named columns feed each
// other's domain fields. It models a same-typed column swap — the mis-map class
// that passes Validate (both targets stay covered, identity transforms) but
// corrupts values. a and b must name columns of the same table that are both
// FromColumn sources in that table's emitters.
func swapTargets(m ShapeMapping, a, b ColumnRef) ShapeMapping {
	out := cloneMapping(m)
	for ti := range out.Tables {
		if out.Tables[ti].Table != a.Table {
			continue
		}
		for _, em := range out.Tables[ti].Emitters {
			var fieldA, fieldB string
			for field, src := range em.Fields {
				fc, ok := src.(FromColumn)
				if !ok {
					continue
				}
				if fc.Column == a.Column {
					fieldA = field
				}
				if fc.Column == b.Column {
					fieldB = field
				}
			}
			if fieldA != "" && fieldB != "" {
				em.Fields[fieldA], em.Fields[fieldB] = em.Fields[fieldB], em.Fields[fieldA]
			}
		}
	}
	return out
}

func hasLaw(r VerifyReport, law ConservationLaw) bool {
	for _, f := range r.Findings {
		if f.Law == law {
			return true
		}
	}
	return false
}
