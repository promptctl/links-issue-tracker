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
// RoleNone kinds (e.g. FocusPath, an ordering fact consumed by sortByFocusPath)
// contribute nothing here. That is an explicit, classified case — not an
// unhandled default — so folding ordering back into membership would require a
// deliberate role change, not a silent omission.

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

