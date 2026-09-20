package cli

import (
	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// queueFacts are the facts about the WHOLE workable queue that a narrowed view
// still has to state truthfully: what closing each row releases, and how many
// rank inversions the queue holds.
//
// Both are inverses or aggregates — answering "what does closing X release"
// for one row means asking every row — so neither can be read off the rows a
// view prints. Deriving them there caps each fact at whatever survived the
// narrowing, and the loss lands on the row that SURVIVED: the prerequisite
// keeps its place in the list with its leverage line silently shortened, under
// a preamble still promising "what closing it would unblock"
// (links-listing-85sd). There is no gap on screen to notice, because the row
// that went missing is the one nobody was looking at.
//
// It is a PROJECTION, not a copy. The queue has to be materialized to derive
// these, but what survives the derivation is a map of ids and an int; the rows
// and their relations are released with the gather that built them, so what a
// command retains is its own view plus a few kilobytes.
// [LAW:polishing-by-subtraction] carry the answer, never the set it came from.
type queueFacts struct {
	unblocks       map[string][]string
	rankInversions int
}

// Unblocks is the ids closing this one would release — direct dependents and
// the ones that inherit the dependency through their epic alike, because
// DependencyIDs names both and closing the prerequisite releases both.
func (f queueFacts) Unblocks(id string) []string { return f.unblocks[id] }

// RankInversions is how many dependencies the queue ranks below their
// dependents. It counts the queue rather than the view because the repair it
// advertises — `lit doctor --fix` — is one made to the repo.
func (f queueFacts) RankInversions() int { return f.rankInversions }

// deriveQueueFacts reads both facts off the whole workable queue in one pass.
// [LAW:dataflow-not-control-flow] one walk, both answers; nothing here asks
// which narrowing the caller went on to apply, because the answers are true
// before any of them run.
func deriveQueueFacts(queue []annotation.AnnotatedIssue) queueFacts {
	facts := queueFacts{unblocks: make(map[string][]string, len(queue))}
	for _, issue := range queue {
		readiness := ClassifyReadiness(issue.Annotations)
		for _, dep := range readiness.DependencyIDs() {
			facts.unblocks[dep] = append(facts.unblocks[dep], issue.ID)
		}
		facts.rankInversions += len(readiness.RankInversions())
	}
	return facts
}

// workableGather is one run of the workable pipeline: the rows the caller asked
// for, the relations behind exactly those rows, the whole-queue facts, and the
// focus scope for a view to narrow by.
//
// [LAW:types-are-the-program] the rows and the queue facts have different types
// and cannot be crossed at a call site. They used to be two
// []annotation.AnnotatedIssue parameters — `issues` and `gathered` — and
// reading the wrong one compiled, ran, and printed a shorter truth.
type workableGather struct {
	rows    []annotation.AnnotatedIssue
	details map[string]storage.IssueRelations
	facts   queueFacts
	scope   focusScope
}

// keepRows narrows the gather to the rows the criteria select and RELEASES
// everything else: the dropped rows' relations go with them, so what this
// returns retains a view, not a queue. The facts derived before the narrowing
// are carried through unchanged, which is the whole point of deriving them
// first. [LAW:one-source-of-truth]
func (g workableGather) keepRows(criteria storage.IssueCriteria) workableGather {
	// Sized for what is kept rather than for what was gathered: hinting the
	// queue's length would leave a filter that keeps ten of 590 rows holding a
	// 590-slot array, which is the queue this says it released.
	var rows []annotation.AnnotatedIssue
	details := map[string]storage.IssueRelations{}
	for _, row := range g.rows {
		if criteria.Selects(row.Issue) {
			rows = append(rows, row)
			details[row.ID] = g.details[row.ID]
		}
	}
	return workableGather{rows: rows, details: details, facts: g.facts, scope: g.scope}
}
