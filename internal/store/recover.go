package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
)

// recover.go is the convergence loop and its three terminal exits — the
// orchestration that turns the .1–.4 seams (DumpRaw, a ShapeMapping producer,
// RebuildCandidate, VerifyCandidate) into autonomous recovery. It composes those
// blocks; it adds no new mechanism of its own. The LLM is fenced out of this
// path entirely: a Mapper is just data-in/data-out, so a deterministic mapper
// and an ambient LLM operator drive the identical loop.

// Mapper proposes a ShapeMapping for a dump, optionally informed by the previous
// pass's repair feedback. It is the one seam between reasoning (an LLM, or a
// deterministic correspondence table) and the mechanical applier+gate.
//
// [LAW:dataflow-not-control-flow] feedback is the datum the loop carries between
// passes — empty on the first. A deterministic mapper is a pure function of the
// dump and ignores it (so it converges in one pass or not at all); a
// feedback-consuming mapper (the LLM) reads it as the self-repair guidance the
// verify gate emitted. The loop does not branch on which kind it holds.
type Mapper func(dump RawDump, feedback string) (ShapeMapping, error)

// DeterministicMapper adapts the built-in correspondence table to the Mapper
// seam. It recognizes the known shapes (clean-ahead, pre-goose→v1) and declines
// everything else — a decline becomes the loop's residual, naming the LLM path
// as the way forward. It ignores feedback: its proposal is a pure function of the
// dump, so re-running it cannot self-repair (run it at maxAttempts=1).
func DeterministicMapper(dump RawDump, _ string) (ShapeMapping, error) {
	m, ok := DeterministicMap(dump)
	if !ok {
		return ShapeMapping{}, errors.New("workspace shape not recognized by any built-in mapper; the LLM mapping path is required (feed `lit lifeboat dump` to the mapper, then apply+verify)")
	}
	return m, nil
}

// RecoveryOutcome is the closed, three-variant result of a recovery run. The set
// is exactly the epic's three exits — there is no fourth, and no variant can
// disagree with itself.
//
// [LAW:types-are-the-program] Each variant carries exactly what its exit needs
// and nothing more, so the illegal combinations are unrepresentable: a converged
// run with no candidate to promote, an unexplained-drop gate with no drops to
// show, a failure with no residual to surface. The discriminator is the variant
// itself — a caller switches on it exhaustively rather than reading flags that
// could contradict each other.
type RecoveryOutcome interface{ isRecoveryOutcome() }

// Reconciled is the autonomous-success exit: a Doctor-clean, conservation-passing
// candidate whose mapping drops nothing unexplained. It is the only exit the
// caller may promote without a human gate. The caller owns Candidate and must
// Discard it once promoted (or abandoned).
type Reconciled struct {
	Candidate *Candidate
	Mapping   ShapeMapping
}

// RequiresDrop is the one normal human gate: a candidate that reconciles, but
// whose mapping discards source columns with no recorded justification. It is
// surfaced before anything commits and is never promoted autonomously. The
// caller owns Candidate and must Discard it.
//
// [LAW:single-enforcer] The gate is a property of the MAPPING (an unexplained
// Dropped), not of the verify report — conservation and provenance are separate
// questions, decided in separate places. Intended drops pass silently; only
// unexplained ones reach here.
type RequiresDrop struct {
	Candidate *Candidate
	Mapping   ShapeMapping
	Drops     []UnexplainedDrop
}

// Unconverged is the loud-failure exit: the attempt budget was exhausted without
// a reconciling rebuild. Residual is the last pass's feedback (a verify report, a
// mapper decline, or an applier rejection) so the failure is never silent.
type Unconverged struct {
	Residual string
	Attempts int
}

func (Reconciled) isRecoveryOutcome()   {}
func (RequiresDrop) isRecoveryOutcome() {}
func (Unconverged) isRecoveryOutcome()  {}

// UnexplainedDrop names one source column the mapping discards without a recorded
// justification — the surface-to-human datum of the RequiresDrop exit.
type UnexplainedDrop struct {
	Column ColumnRef
}

// Recover runs the recovery loop to one of the three exits. It builds each
// candidate adjacent to the canonical workspace (so a later promotion is an
// atomic same-filesystem rename) and verifies it against the immutable dump.
//
// [LAW:dataflow-not-control-flow] Every pass runs the identical attempt; the only
// variability between passes is the feedback datum carried forward. The loop
// terminates the instant an attempt reconciles, or surfaces the last feedback
// when the budget is spent — there is no give-up that drops the residual.
//
// [LAW:one-source-of-truth] dump is read-only across the whole loop; every pass
// reuses it unchanged, so attempt N's conservation check cannot be contaminated
// by attempt N-1 (each rejected candidate is discarded whole before the next).
func Recover(ctx context.Context, canonicalDoltDir string, dump RawDump, mapper Mapper, maxAttempts int) (RecoveryOutcome, error) {
	// [LAW:types-are-the-program] At least one pass must run, or Unconverged would
	// carry an empty residual — making "failure with no residual" representable and
	// breaking the variant's contract. The budget is a caller-supplied programming
	// value, so a non-positive one is a precondition violation surfaced loudly, not
	// a silent zero-pass exit.
	if maxAttempts < 1 {
		return nil, fmt.Errorf("recovery attempt budget must be at least 1, got %d", maxAttempts)
	}
	// [LAW:single-enforcer] Validate and canonicalize the path at this boundary —
	// the same check Open/DumpRaw enforce. An empty path would stage candidates
	// under "."; a trailing separator would make filepath.Dir return the canonical
	// dir itself, staging candidates INSIDE it so a later promotion rename (source
	// becomes a descendant of destination) fails. The cleaned form derives a parent
	// reliably adjacent to the canonical workspace.
	canonicalDoltDir, err := validateDoltRootDir(canonicalDoltDir)
	if err != nil {
		return nil, err
	}
	parentDir := filepath.Dir(canonicalDoltDir)
	feedback := ""
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		outcome, next, err := runAttempt(ctx, parentDir, dump, mapper, feedback)
		if err != nil {
			return nil, err
		}
		if outcome != nil {
			return outcome, nil
		}
		feedback = next
	}
	return Unconverged{Residual: feedback, Attempts: maxAttempts}, nil
}

// runAttempt is one map→build→verify pass. It yields exactly one of: a terminal
// outcome (the candidate reconciled — Reconciled or RequiresDrop), repair
// feedback for the next pass, or a hard error (the gate could not run, or a
// rejected candidate could not be discarded — either leaves the loop unable to
// proceed cleanly).
//
// [LAW:dataflow-not-control-flow] The pass is a fixed pipeline; each stage either
// advances or turns its failure into the feedback datum. A non-reconciling
// candidate is discarded here, so the loop never carries residue forward — the
// next pass starts from an empty tree.
func runAttempt(ctx context.Context, parentDir string, dump RawDump, mapper Mapper, feedback string) (RecoveryOutcome, string, error) {
	mapping, err := mapper(dump, feedback)
	if err != nil {
		return nil, fmt.Sprintf("the mapper could not propose a mapping: %v", err), nil
	}
	cand, err := RebuildCandidate(ctx, parentDir, dump, mapping)
	if err != nil {
		// [LAW:no-silent-fallbacks] Only a mapping rejection is self-repairable
		// feedback. A build failure past a valid mapping — filesystem, store I/O,
		// corrupt source data — cannot be fixed by re-mapping, so it surfaces as a
		// hard error rather than being relabeled as feedback and silently burning
		// the retry budget while hiding the real cause.
		if errors.Is(err, ErrInvalidMapping) {
			return nil, fmt.Sprintf("the proposed mapping was rejected by the applier: %v", err), nil
		}
		return nil, "", fmt.Errorf("rebuild candidate from a valid mapping failed: %w", err)
	}
	report, err := VerifyCandidate(ctx, dump, mapping, cand.Store())
	if err != nil {
		return nil, "", errors.Join(fmt.Errorf("verify gate could not run: %w", err), cand.Discard())
	}
	if !report.Reconciled() {
		if discErr := cand.Discard(); discErr != nil {
			return nil, "", fmt.Errorf("discard rejected candidate: %w", discErr)
		}
		return nil, report.String(), nil
	}
	return classifyConverged(cand, mapping), "", nil
}

// classifyConverged splits a reconciled candidate into its two converged exits on
// the one discriminator that distinguishes them: whether the mapping that built
// it discards anything unexplained.
func classifyConverged(cand *Candidate, mapping ShapeMapping) RecoveryOutcome {
	drops := unexplainedDrops(mapping)
	if len(drops) > 0 {
		return RequiresDrop{Candidate: cand, Mapping: mapping, Drops: drops}
	}
	return Reconciled{Candidate: cand, Mapping: mapping}
}

// unexplainedDrops collects the columns the mapping discards without a recorded
// justification, in deterministic order. [LAW:single-enforcer] The provenance
// discriminator is read straight off the Dropped disposition the mapper produced;
// this does not re-derive "why was it dropped".
func unexplainedDrops(m ShapeMapping) []UnexplainedDrop {
	var out []UnexplainedDrop
	for _, tm := range m.Tables {
		for col, d := range tm.Drops {
			if d.Provenance == DropUnexplained {
				out = append(out, UnexplainedDrop{Column: ColumnRef{Table: tm.Table, Column: col}})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Column.String() < out[j].Column.String() })
	return out
}
