package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/rank"
)

// The verify gate is the trust boundary the recovery loop cannot talk its way
// past: a rebuilt candidate is promoted only when it is both internally healthy
// AND conserves the source dump. The LLM (or a deterministic mapper) proposes a
// ShapeMapping; the applier builds a candidate; this gate decides whether to
// trust it. The LLM expands what is RECOVERABLE without expanding what is
// TRUSTED — it has no influence here.
//
// [LAW:single-enforcer] The health half IS Doctor(): the candidate is healthy
// exactly when it passes the same invariant suite (constraint integrity, foreign
// keys, dependency cycles) every live workspace must pass. This gate calls
// Doctor rather than recomputing those checks, so the two cannot drift. Foreign
// keys ARE the "every relation endpoint resolves" conservation law — owned there,
// not duplicated here.
//
// Layered on top, the conservation half answers the question Doctor structurally
// cannot — "did we lose or silently mis-map data VERSUS the source?" — through
// laws of increasing depth (count, id stability, rank permutation).
//
// [LAW:types-are-the-program] A conservation law can only be a TRUE theorem if it
// is non-circular: comparing the dump re-derived through the mapping against a
// candidate built through that same mapping is a tautology that passes any
// mis-map, because the mapping is the shared input to both sides. So every law
// here compares against something the mapping cannot corrupt — either the dump's
// RAW shape (row counts, raw id cells, read end-to-end through the write path) or
// an INTRINSIC invariant any faithful rebuild must satisfy regardless of
// provenance (ranks forming a valid permutation). This is the honest ceiling: a
// swap between two same-typed free-text fields is undetectable without guessing,
// and the gate does not guess. A mis-map is caught precisely when it makes the
// data violate one of these theorems — e.g. priority values landing in rank
// collide, breaking distinctness.

// ConservationLaw names which invariant a finding violates. It is the
// machine-checkable discriminator of a discrepancy; the closed set is the verify
// suite — the Doctor health half plus the conservation laws layered on it.
type ConservationLaw string

const (
	// LawHealth: a Doctor error (the health half). Doctor owns the wording.
	LawHealth ConservationLaw = "health"
	// LawCount: per-collection count conservation — a source table mapped into a
	// collection contributed a different number of records than it had rows.
	LawCount ConservationLaw = "count"
	// LawIDStability: the set of issue ids changed across the rebuild.
	LawIDStability ConservationLaw = "id_stability"
	// LawRank: the rebuilt ranks are not a valid permutation (a strict total
	// order requires one well-formed, distinct rank per ranked issue).
	LawRank ConservationLaw = "rank_permutation"
)

// VerifyFinding is one discrepancy. [LAW:errors] It is dual-purpose by
// construction: Law is the machine-checkable discriminator a caller branches on,
// and Detail is the localized human/LLM sentence — it names the exact ids, ranks
// or collections unaccounted for, so the same value drives both an assertion and
// the next repair prompt. The locus lives inside Detail rather than in a union of
// per-law optional fields, because each law localizes differently (counts name a
// collection, id stability names ids, rank names colliding ranks) and a shared
// bag-of-optionals would be mostly empty for every finding.
type VerifyFinding struct {
	Law    ConservationLaw `json:"law"`
	Detail string          `json:"detail"`
}

// VerifyReport is the gate's verdict. [LAW:types-are-the-program] Emptiness IS
// the accept state — Reconciled is not a separate flag that could disagree with
// the findings, it is derived from them — so "accepted but with discrepancies"
// is unrepresentable. A non-conserved candidate is NOT an error: it is the normal
// reject path whose findings become the loop's next guidance. An error is
// returned only when the gate itself could not run (Doctor or Export failed).
type VerifyReport struct {
	Findings []VerifyFinding `json:"findings"`
}

// Reconciled reports the autonomous-success exit: Doctor-clean and every
// conservation law holds.
func (r VerifyReport) Reconciled() bool { return len(r.Findings) == 0 }

// String renders the report as the loop's repair prompt: a reconciled report
// states success; otherwise every finding is listed with its law tag so the
// reader (LLM or human) sees exactly what is unaccounted for.
func (r VerifyReport) String() string {
	if r.Reconciled() {
		return "verify: reconciled — Doctor-clean and all conservation laws hold"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "verify: %d discrepancy(ies) — the rebuild does not conserve the source and cannot be trusted:\n", len(r.Findings))
	for i, f := range r.Findings {
		fmt.Fprintf(&b, "  %d. [%s] %s\n", i+1, f.Law, f.Detail)
	}
	return b.String()
}

// VerifyCandidate is the gate. It reads the candidate store read-only (Doctor +
// Export) and judges it against the immutable source dump under the mapping that
// built it.
//
// [LAW:dataflow-not-control-flow] Every law runs on every call; whether each
// produces a finding is decided by the data (a count delta, a missing id, a
// duplicate rank), never by branching around the check. The findings accumulate
// into one report so a single feedback artifact carries every reason at once.
func VerifyCandidate(ctx context.Context, dump RawDump, mapping ShapeMapping, st *Store) (VerifyReport, error) {
	health, err := st.Doctor(ctx)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("verify health gate (doctor): %w", err)
	}
	export, err := st.Export(ctx)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("verify conservation gate (export): %w", err)
	}

	var findings []VerifyFinding
	findings = append(findings, healthFindings(health)...)
	findings = append(findings, countFindings(dump, mapping, export)...)
	findings = append(findings, idStabilityFindings(dump, mapping, export)...)
	findings = append(findings, rankFindings(export)...)
	return VerifyReport{Findings: findings}, nil
}

// healthFindings folds Doctor's verdict into the report. [LAW:single-enforcer]
// Doctor already classifies severity: its Errors are integrity/foreign-key
// failures that make a workspace untrustworthy, its Warnings (rank inversions,
// related-ordering) are conditions a faithful rebuild of slightly-messy source
// legitimately reproduces. Rejecting on warnings would make recovering such a
// source impossible, so only Errors become findings — and the classification is
// Doctor's, not a second opinion here.
func healthFindings(h HealthReport) []VerifyFinding {
	out := make([]VerifyFinding, 0, len(h.Errors))
	for _, e := range h.Errors {
		out = append(out, VerifyFinding{Law: LawHealth, Detail: e})
	}
	return out
}

// conservationCollections is the fixed order findings are reported in, so a
// report is deterministic regardless of map iteration order.
var conservationCollections = []collection{
	collIssues, collRelations, collComments, collLabels, collEvents, collEventChanges,
}

// countFindings checks count conservation per collection. [LAW:types-are-the-program]
// The reference is the RAW dump — len(table.Rows) — never the mapping re-applied,
// so this measures the end-to-end round trip (apply, write, read back) for row
// loss or duplication, which no value-via-mapping comparison could (that would be
// circular). A fully-dropped table maps into no collection (tableTarget returns
// no columns), so it is naturally excluded — the "modulo explicitly-dropped
// fields" clause needs no special case. event_changes are nested under their
// events, so their conserved count is the total of nested changes.
func countFindings(dump RawDump, m ShapeMapping, export model.Export) []VerifyFinding {
	expected := map[collection]int{}
	for _, table := range dump.Tables {
		coll, mapped := tableTarget(table, m)
		if len(mapped) == 0 {
			continue
		}
		expected[coll] += len(table.Rows)
	}

	nestedChanges := 0
	for _, ev := range export.Events {
		nestedChanges += len(ev.Changes)
	}
	actual := map[collection]int{
		collIssues:       len(export.Issues),
		collRelations:    len(export.Relations),
		collComments:     len(export.Comments),
		collLabels:       len(export.Labels),
		collEvents:       len(export.Events),
		collEventChanges: nestedChanges,
	}

	var out []VerifyFinding
	for _, coll := range conservationCollections {
		if expected[coll] != actual[coll] {
			out = append(out, VerifyFinding{
				Law:    LawCount,
				Detail: fmt.Sprintf("collection %q: source dump carries %d row(s) mapped here, rebuild has %d", coll, expected[coll], actual[coll]),
			})
		}
	}
	return out
}

// idStabilityFindings checks that issue ids survive the rebuild unchanged.
// [LAW:types-are-the-program] The reference is the RAW cells of whichever source
// column maps onto issues.id — not the candidate re-derived through the mapping —
// so comparing that raw set to the ids read back catches identity loss in the
// write path (a collision silently merging two rows, a value the write path
// rewrote). It cannot catch the id column itself being mis-identified, because no
// deterministic check knows which source column "should" be the id without
// guessing; that is the honest ceiling, and count conservation guards the
// adjacent row-loss case. Validate guarantees at most one column maps to a given
// target, so the first match is the only match.
func idStabilityFindings(dump RawDump, m ShapeMapping, export model.Export) []VerifyFinding {
	sourceIDs, mapped := sourceValuesFor(dump, m, "issues.id")
	if !mapped {
		// No source column maps to issues.id: the dump carries no issues to
		// conserve. Count conservation owns the (empty) issues collection.
		return nil
	}

	source := map[string]bool{}
	for _, id := range sourceIDs {
		source[id] = true
	}
	rebuilt := map[string]bool{}
	for _, issue := range export.Issues {
		rebuilt[issue.ID] = true
	}

	missing := setDifference(source, rebuilt)
	extra := setDifference(rebuilt, source)

	var out []VerifyFinding
	if len(missing) > 0 {
		out = append(out, VerifyFinding{
			Law:    LawIDStability,
			Detail: fmt.Sprintf("%d issue id(s) present in the source dump but absent from the rebuild: %s", len(missing), strings.Join(missing, ", ")),
		})
	}
	if len(extra) > 0 {
		out = append(out, VerifyFinding{
			Law:    LawIDStability,
			Detail: fmt.Sprintf("%d issue id(s) present in the rebuild but absent from the source dump: %s", len(extra), strings.Join(extra, ", ")),
		})
	}
	return out
}

// rankFindings checks that the rebuilt ranks form a valid permutation: among
// ranked issues, every rank is a well-formed lexorank value and all are distinct,
// which is exactly a strict total order over those issues.
//
// [LAW:types-are-the-program] This is the intrinsic, mapping-independent law, and
// it is what catches a value mis-map the round-trip laws cannot: priority values
// landing in rank ("0", "1", ...) collide, so distinctness fails. Empty ranks are
// unranked issues — legitimately many — so only non-empty ranks are checked.
func rankFindings(export model.Export) []VerifyFinding {
	idsByRank := map[string][]string{}
	var malformed []string
	for _, issue := range export.Issues {
		if issue.Rank == "" {
			continue
		}
		if !rank.Valid(issue.Rank) {
			malformed = append(malformed, fmt.Sprintf("%s=%q", issue.ID, issue.Rank))
		}
		idsByRank[issue.Rank] = append(idsByRank[issue.Rank], issue.ID)
	}

	var collisions []string
	for r, ids := range idsByRank {
		if len(ids) > 1 {
			sort.Strings(ids)
			collisions = append(collisions, fmt.Sprintf("%q shared by %s", r, strings.Join(ids, ", ")))
		}
	}
	sort.Strings(collisions)
	sort.Strings(malformed)

	var out []VerifyFinding
	if len(collisions) > 0 {
		out = append(out, VerifyFinding{
			Law:    LawRank,
			Detail: fmt.Sprintf("ranks are not distinct (a valid order needs one rank per issue): %s", strings.Join(collisions, "; ")),
		})
	}
	if len(malformed) > 0 {
		out = append(out, VerifyFinding{
			Law:    LawRank,
			Detail: fmt.Sprintf("%d issue(s) carry a value that is not a well-formed rank: %s", len(malformed), strings.Join(malformed, ", ")),
		})
	}
	return out
}

// sourceValuesFor returns the raw cell values of the source column mapped onto
// target, as strings, and whether such a column exists. It is the raw-dump
// reference the round-trip conservation laws compare against.
func sourceValuesFor(dump RawDump, m ShapeMapping, target TargetKey) ([]string, bool) {
	for _, table := range dump.Tables {
		for i, col := range table.Columns {
			mapped, ok := m.Columns[ColumnRef{Table: table.Name, Column: col}].(MappedTo)
			if !ok || mapped.Target != target {
				continue
			}
			values := make([]string, 0, len(table.Rows))
			for _, row := range table.Rows {
				values = append(values, cellString(row[i]))
			}
			return values, true
		}
	}
	return nil, false
}

// setDifference returns the sorted members of a not present in b.
func setDifference(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
