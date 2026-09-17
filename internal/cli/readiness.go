package cli

import (
	"github.com/promptctl/links-issue-tracker/internal/annotation"
)

// This file is the annotation→policy boundary. Each annotation kind declares
// its readiness ROLE where the kind is defined (annotation.ReadinessRole);
// ClassifyReadiness is the ONLY place those roles are INTERPRETED into a pull
// decision — every ready/blocked/orphaned/inversion consumer reads the typed
// result instead of re-walking annotation.Kind.
// [LAW:single-enforcer] Single annotation→readiness interpreter.
//
// RoleNone kinds (e.g. FocusPath, the view-scope fact) contribute nothing here.
// That is an explicit, classified case — not an unhandled default — so folding
// view scope back into readiness would require a deliberate role change, not a
// silent omission.

// BlockingReason is one classified fact that prevents pulling an issue now.
// Detail carries the annotation message: the missing field name, the open
// dependency id (direct or inherited), the pending sibling id, or the
// needs-design label.
type BlockingReason struct {
	Kind   annotation.Kind
	Detail string
}

// Phrase renders one blocking reason as the words a reader sees. Every surface
// that names a blocker reads this one vocabulary: the backlog's "blocked:"
// line (minus open dependencies, which it gives their own line) and the epic
// plan's blocked marker.
// [LAW:one-source-of-truth] A second surface phrasing kinds for itself is a
// second list to keep current, and the shorter of the two lists is always the
// one nobody notices — the epic plan carried exactly that gap.
// [LAW:no-silent-failure] The default panics rather than rendering a blocking
// kind as empty text: a fifth kind must fail loudly here instead of arriving on
// screen as a blank reason or, worse, no reason at all.
func (r BlockingReason) Phrase() string {
	if label, ok := r.dependency(); ok {
		return "depends on " + label
	}
	switch r.Kind {
	case annotation.MissingField:
		return "missing " + r.Detail
	case annotation.NeedsDesign:
		return NeedsDesignLabel
	case annotation.EarlierSiblingPending:
		return "earlier sibling " + r.Detail + " still open"
	default:
		panic("BlockingReason.Phrase: blocking kind with no phrasing: " + r.Kind.String())
	}
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

// dependency answers whether this reason is an unfinished issue gating the one
// classified — declared on it directly, or inherited from an epic it sits under
// — and, when it is, how a reader sees it. The two kinds are one fact for
// everything that acts on it (closing the id discharges either), so they share
// every dependency surface; they differ only in where the edge lives, which is
// the one thing the label adds, because the remedy for an inherited edge is on
// the epic, never on this issue.
// [LAW:one-type-per-behavior] [LAW:one-source-of-truth] the one list of which
// kinds are dependencies, read by the ids, the labels, and Phrase alike.
func (r BlockingReason) dependency() (label string, ok bool) {
	switch r.Kind {
	case annotation.OpenDependency:
		return r.Detail, true
	case annotation.InheritedDependency:
		return r.Detail + " (via epic)", true
	}
	return "", false
}

// DependencyIDs returns the ids of the dependencies among the blocking reasons,
// direct and inherited alike: the ids whose closing unblocks this issue.
func (r IssueReadiness) DependencyIDs() []string {
	var ids []string
	for _, reason := range r.blocking {
		if _, ok := reason.dependency(); ok {
			ids = append(ids, reason.Detail)
		}
	}
	return ids
}

// DependencyLabels returns the same dependencies as DependencyIDs, in the same
// order, worded for the "depends on:" lines a reader sees.
func (r IssueReadiness) DependencyLabels() []string {
	var labels []string
	for _, reason := range r.blocking {
		if label, ok := reason.dependency(); ok {
			labels = append(labels, label)
		}
	}
	return labels
}

// ClassifyReadiness interprets an issue's annotations into a typed readiness
// classification. Pure: one pass over the values, no store access.
// [LAW:dataflow-not-control-flow] Every annotation flows through the same
// classification, keyed on its declared readiness role; RoleNone contributes
// nothing as an explicit case, not a caller-side skip.
// [LAW:no-silent-failure] The default panics: every registry kind has a valid
// role, so the only way here is a zero/corrupt kind — surfaced loudly rather
// than defaulting to ready (the exact silent path this seam used to have).
func ClassifyReadiness(anns []annotation.Annotation) IssueReadiness {
	var r IssueReadiness
	for _, a := range anns {
		switch a.Kind.ReadinessRole() {
		case annotation.RoleBlocking:
			r.blocking = append(r.blocking, BlockingReason{Kind: a.Kind, Detail: a.Message})
		case annotation.RoleOrphaned:
			r.orphaned = true
		case annotation.RoleRankInversion:
			r.rankInversions = append(r.rankInversions, a.Message)
		case annotation.RoleNone:
			// ordering/advisory fact; deliberately invisible to readiness
		default:
			panic("ClassifyReadiness: annotation carries an unclassified kind: " + a.Kind.String())
		}
	}
	return r
}
