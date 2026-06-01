package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// canonicalUnder returns a canonical Dolt path whose parent exists, so Recover
// can stage candidates beside it. Recover never promotes, so the path itself need
// not exist.
func canonicalUnder(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "dolt")
}

// cloneMapping copies a mapping so a test can perturb one column without mutating
// the shared deterministic proposal.
func cloneMapping(m ShapeMapping) ShapeMapping {
	out := ShapeMapping{Columns: make(map[ColumnRef]Disposition, len(m.Columns))}
	for ref, disp := range m.Columns {
		out.Columns[ref] = disp
	}
	return out
}

// staticMapper is the feedback-ignoring Mapper a test uses to drive the loop with
// one fixed proposal (the deterministic mapper's nature, made explicit).
func staticMapper(m ShapeMapping) Mapper {
	return func(RawDump, string) (ShapeMapping, error) { return m, nil }
}

// TestRecoverReconcilesKnownShape is acceptance #1: a recognized shape recovers
// autonomously — the loop reaches Reconciled with a Doctor-clean, conserving
// candidate and no human input.
func TestRecoverReconcilesKnownShape(t *testing.T) {
	ctx := context.Background()
	dump := preGooseDump()

	outcome, err := Recover(ctx, canonicalUnder(t), dump, DeterministicMapper, 1)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	recon, ok := outcome.(Reconciled)
	if !ok {
		t.Fatalf("want Reconciled, got %T: %+v", outcome, outcome)
	}
	t.Cleanup(func() { _ = recon.Candidate.Discard() })

	report, err := recon.Candidate.Store().Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	mustClean(t, report)

	export, err := recon.Candidate.Store().Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(export.Issues) != 2 {
		t.Fatalf("issue conservation broken: want 2, got %d", len(export.Issues))
	}
}

// TestRecoverSelfRepairsAcrossAttempts proves the loop's defining property: a
// non-reconciling pass feeds its diagnosis forward, and a feedback-consuming
// mapper converges on a later pass. The first proposal is malformed (the applier
// rejects it); the second is correct and must arrive carrying the prior feedback.
func TestRecoverSelfRepairsAcrossAttempts(t *testing.T) {
	ctx := context.Background()
	dump := preGooseDump()
	good := mustMap(t, dump)

	calls := 0
	mapper := func(_ RawDump, feedback string) (ShapeMapping, error) {
		calls++
		if calls == 1 {
			if feedback != "" {
				t.Errorf("first pass should receive no feedback, got %q", feedback)
			}
			// A non-total mapping: the applier rejects it before any rebuild.
			return ShapeMapping{Columns: map[ColumnRef]Disposition{}}, nil
		}
		if feedback == "" {
			t.Error("second pass should receive the prior pass's repair feedback, got none")
		}
		return good, nil
	}

	outcome, err := Recover(ctx, canonicalUnder(t), dump, mapper, 3)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, ok := outcome.(Reconciled); !ok {
		t.Fatalf("want Reconciled after self-repair, got %T", outcome)
	}
	if calls != 2 {
		t.Fatalf("want convergence on the 2nd pass, mapper called %d time(s)", calls)
	}
	outcome.(Reconciled).Candidate.Discard()
}

// TestRecoverRequiresDropOnUnexplainedDrop is acceptance #2: a rebuild that
// reconciles but whose mapping discards a column with no justification reaches the
// RequiresDrop human gate, naming the dropped column — and does NOT silently
// promote. The dropped column is optional, so the rebuild still conserves; the
// drop is caught by provenance, not by the conservation gate.
func TestRecoverRequiresDropOnUnexplainedDrop(t *testing.T) {
	ctx := context.Background()
	dump := preGooseDump()

	mapping := cloneMapping(mustMap(t, dump))
	dropped := ColumnRef{Table: "issues", Column: "assignee"}
	mapping.Columns[dropped] = Dropped{Provenance: DropUnexplained}

	outcome, err := Recover(ctx, canonicalUnder(t), dump, staticMapper(mapping), 1)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	rd, ok := outcome.(RequiresDrop)
	if !ok {
		t.Fatalf("want RequiresDrop, got %T: %+v", outcome, outcome)
	}
	t.Cleanup(func() { _ = rd.Candidate.Discard() })

	if len(rd.Drops) != 1 || rd.Drops[0].Column != dropped {
		t.Fatalf("want the unexplained drop %s surfaced, got %+v", dropped, rd.Drops)
	}
}

// TestRecoverUnconvergedSurfacesResidual is acceptance #3: when the budget is
// exhausted without reconciling, the loop fails loudly carrying the last verify
// report — never a silent give-up. The mis-map (priority<->rank) is well-formed
// but breaks rank distinctness, so every pass produces the same finding.
func TestRecoverUnconvergedSurfacesResidual(t *testing.T) {
	ctx := context.Background()
	dump := rankedDump()
	swapped := swapTargets(mustMap(t, dump),
		ColumnRef{Table: "issues", Column: "priority"}, ColumnRef{Table: "issues", Column: "item_rank"})

	outcome, err := Recover(ctx, canonicalUnder(t), dump, staticMapper(swapped), 3)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	un, ok := outcome.(Unconverged)
	if !ok {
		t.Fatalf("want Unconverged, got %T: %+v", outcome, outcome)
	}
	if un.Attempts != 3 {
		t.Fatalf("want Attempts=3, got %d", un.Attempts)
	}
	if !strings.Contains(un.Residual, string(LawRank)) {
		t.Fatalf("residual must carry the verify report's rank finding, got:\n%s", un.Residual)
	}
}

// TestRecoverRejectsNonPositiveBudget guards Unconverged's residual contract: a
// budget below one would exit with no pass run and an empty residual, making
// "failure with no residual" representable. The precondition fails loudly instead.
func TestRecoverRejectsNonPositiveBudget(t *testing.T) {
	ctx := context.Background()
	dump := preGooseDump()
	for _, n := range []int{0, -1} {
		outcome, err := Recover(ctx, canonicalUnder(t), dump, DeterministicMapper, n)
		if err == nil {
			t.Fatalf("maxAttempts=%d must error, got outcome %T", n, outcome)
		}
		if outcome != nil {
			t.Fatalf("maxAttempts=%d must return no outcome, got %T", n, outcome)
		}
	}
}

// TestRecoveryEntryPointsRejectEmptyPath locks the shared trust boundary: every
// exported recovery entry refuses an empty canonical path rather than degrading to
// cwd-relative scratch, lock, and backup artifacts.
func TestRecoveryEntryPointsRejectEmptyPath(t *testing.T) {
	ctx := context.Background()
	if _, err := Recover(ctx, "  ", preGooseDump(), DeterministicMapper, 1); err == nil {
		t.Error("Recover must reject an empty canonical path")
	}
	if _, err := PromoteCandidate(ctx, "  ", nil); err == nil {
		t.Error("PromoteCandidate must reject an empty canonical path")
	}
	if err := HealWorkspace(ctx, "  "); err == nil {
		t.Error("HealWorkspace must reject an empty canonical path")
	}
}

// TestRebuildCandidateTagsMappingRejection locks the discriminator the loop relies
// on: a mapping the applier rejects is tagged ErrInvalidMapping, so the loop can
// route it back as feedback while leaving every other (infrastructure) build
// failure to surface as a hard error.
func TestRebuildCandidateTagsMappingRejection(t *testing.T) {
	ctx := context.Background()
	_, err := RebuildCandidate(ctx, t.TempDir(), preGooseDump(), ShapeMapping{Columns: map[ColumnRef]Disposition{}})
	if err == nil {
		t.Fatal("a non-total mapping must be rejected")
	}
	if !errors.Is(err, ErrInvalidMapping) {
		t.Fatalf("mapping rejection must be tagged ErrInvalidMapping, got: %v", err)
	}
}

// TestRecoverUnconvergedOnPersistentMapperDecline covers the other residual
// source: a mapper that never produces a proposal (e.g. an unrecognized shape)
// exhausts the budget with its decline as the residual, not a panic or silence.
func TestRecoverUnconvergedOnPersistentMapperDecline(t *testing.T) {
	ctx := context.Background()
	mapper := func(RawDump, string) (ShapeMapping, error) {
		return ShapeMapping{}, errors.New("shape not recognized")
	}

	outcome, err := Recover(ctx, canonicalUnder(t), preGooseDump(), mapper, 2)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	un, ok := outcome.(Unconverged)
	if !ok {
		t.Fatalf("want Unconverged, got %T", outcome)
	}
	if un.Attempts != 2 || !strings.Contains(un.Residual, "shape not recognized") {
		t.Fatalf("residual must carry the mapper's decline; got attempts=%d residual=%q", un.Attempts, un.Residual)
	}
}
