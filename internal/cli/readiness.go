package cli

import (
	"github.com/promptctl/links-issue-tracker/internal/annotation"
)

// This file is the annotation→policy boundary. Annotations are neutral facts
// (per project invariant); readiness is an interpretation a consumer applies
// over them. ClassifyReadiness is the ONLY place that interpretation happens —
// every ready/blocked/orphaned/inversion consumer reads the typed result
// instead of re-walking annotation.Kind.
// [LAW:single-enforcer] Single annotation→readiness interpreter.
//
// FocusPath is deliberately NOT classified here. It is an ordering fact
// (sortByFocusPath), never a membership or readiness one; folding it into the
// classifier would re-tangle ordering with readiness — the exact entanglement
// the focus feature was designed to avoid. ClassifyReadiness treats it as it
// treats any unknown-to-policy kind: invisible.

// blockingKinds enumerates the annotation kinds that block readiness.
// [LAW:one-source-of-truth] The single definition of what "blocks readiness";
// both classification and the summary's display order read it.
var blockingKinds = []annotation.Kind{
	annotation.MissingField,
	annotation.OpenDependency,
	annotation.NeedsDesign,
	annotation.EarlierSiblingPending,
}

func isBlockingKind(kind annotation.Kind) bool {
	for _, k := range blockingKinds {
		if kind == k {
			return true
		}
	}
	return false
}

// BlockingReason is one classified fact that prevents pulling an issue now.
// Detail carries the annotation message: the missing field name, the open
// dependency id, the pending sibling id, or the needs-design label.
type BlockingReason struct {
	Kind   annotation.Kind
	Detail string
}

// IssueReadiness is the sealed readiness classification of one issue's
// annotations. The three fields mirror the three interpretation families:
// membership (blocking), staleness (orphaned), and rank hygiene (inversions).
// Only ClassifyReadiness produces values; consumers read the methods.
// [LAW:types-are-the-program] Ready-ness is len(blocking)==0 by construction —
// emptiness of the raw annotation slice is never a proxy for ready.
type IssueReadiness struct {
	blocking       []BlockingReason
	orphaned       bool
	rankInversions []string
}

// IsReady reports whether nothing blocks pulling the issue. Status gating
// (e.g. "don't start an in_progress leaf") stays at the consumer — readiness
// speaks only for the annotation facts.
func (r IssueReadiness) IsReady() bool { return len(r.blocking) == 0 }

// BlockingReasons returns the classified blocking facts, in annotation order.
func (r IssueReadiness) BlockingReasons() []BlockingReason { return r.blocking }

// IsOrphaned reports whether the issue carries the orphaned staleness fact.
func (r IssueReadiness) IsOrphaned() bool { return r.orphaned }

// RankInversions returns the ids of dependencies ranked below this issue.
func (r IssueReadiness) RankInversions() []string { return r.rankInversions }

// DependencyIDs returns the open-dependency ids among the blocking reasons —
// the same facts display surfaces render as "depends on:" lines.
func (r IssueReadiness) DependencyIDs() []string {
	var ids []string
	for _, reason := range r.blocking {
		if reason.Kind == annotation.OpenDependency {
			ids = append(ids, reason.Detail)
		}
	}
	return ids
}

// ClassifyReadiness interprets an issue's neutral annotations into a typed
// readiness classification. Pure: one pass over the values, no store access.
// [LAW:dataflow-not-control-flow] Every annotation flows through the same
// classification; non-policy kinds (FocusPath) contribute nothing rather than
// being skipped by a caller-side branch.
func ClassifyReadiness(anns []annotation.Annotation) IssueReadiness {
	var r IssueReadiness
	for _, a := range anns {
		switch {
		case isBlockingKind(a.Kind):
			r.blocking = append(r.blocking, BlockingReason{Kind: a.Kind, Detail: a.Message})
		case a.Kind == annotation.Orphaned:
			r.orphaned = true
		case a.Kind == annotation.RankInversion:
			r.rankInversions = append(r.rankInversions, a.Message)
		}
	}
	return r
}

// kindCount pairs a blocking kind with the number of issues blocked by it.
type kindCount struct {
	Kind  annotation.Kind
	Count int
}

// blockingKindCounts aggregates classified issues into per-kind blocked
// counts, in the canonical blockingKinds order. An issue with several reasons
// of one kind counts once for that kind.
func blockingKindCounts(rs []IssueReadiness) []kindCount {
	counts := map[annotation.Kind]int{}
	for _, r := range rs {
		seen := map[annotation.Kind]bool{}
		for _, reason := range r.blocking {
			if seen[reason.Kind] {
				continue
			}
			seen[reason.Kind] = true
			counts[reason.Kind]++
		}
	}
	var out []kindCount
	for _, kind := range blockingKinds {
		if n := counts[kind]; n > 0 {
			out = append(out, kindCount{Kind: kind, Count: n})
		}
	}
	return out
}
