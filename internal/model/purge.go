package model

import (
	"slices"
	"strings"
	"time"
)

// Purge records that an issue was permanently removed: its row, its comments,
// its labels, its history, and every edge to or from it are gone, and this
// record is the only thing left that names it. It holds the id and the act,
// never the content — the way a git history rewrite leaves a sha pasted into a
// commit message elsewhere while the object it named is gone.
//
// The record is what makes a purge reach other stores. A row that is merely
// absent reads, in a three-way merge, as "removed on one side", and a
// concurrent edit on the other side wins over a removal; a purge record wins
// over everything, because every merge applies the union of the purge sets it
// sees before it compares a single row. [LAW:types-are-the-program] the
// purge is a fact that travels, not an absence each store has to infer.
type Purge struct {
	IssueID  string    `json:"issue_id"`
	PurgedAt time.Time `json:"purged_at"`
	PurgedBy string    `json:"purged_by"`
}

// UnionPurges combines purge sets into one, by issue id ascending. A set only
// ever grows — nothing un-purges — so the union of two stores' sets is the set
// both converge on. Two stores that purged one id independently each hold a
// record of it; the earlier purge is kept, the actor breaking a same-instant
// tie, so every store keeps the same record whatever order the sets arrive in.
func UnionPurges(sets ...[]Purge) []Purge {
	byID := map[string]Purge{}
	for _, set := range sets {
		for _, p := range set {
			held, ok := byID[p.IssueID]
			if !ok || purgeOrder(p, held) < 0 {
				byID[p.IssueID] = p
			}
		}
	}
	out := make([]Purge, 0, len(byID))
	for _, p := range byID {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Purge) int { return strings.Compare(a.IssueID, b.IssueID) })
	return out
}

// purgeOrder ranks two records of one id: earlier purge first, then actor.
func purgeOrder(a, b Purge) int {
	if c := a.PurgedAt.Compare(b.PurgedAt); c != 0 {
		return c
	}
	return strings.Compare(a.PurgedBy, b.PurgedBy)
}

// ApplyPurges returns the export with purges as its purge set and no row that
// belongs to a purged issue: the issue itself, every relation with a purged
// endpoint, and the purged issues' comments, labels and history. Rows of other
// issues stay byte-for-byte, so an id another ticket mentions in its text, its
// follows_up, or its redirect target keeps the mention.
//
// It is the one place "a purged id holds no rows" is applied to an export.
// The merge applies it to all three sides before comparing anything, and a
// restore applies it with the store's own purges folded in, so neither a
// concurrent edit nor an old backup can bring a purged ticket back.
// [LAW:single-enforcer]
func (e Export) ApplyPurges(purges []Purge) Export {
	purged := make(map[string]bool, len(purges))
	for _, p := range purges {
		purged[p.IssueID] = true
	}
	kept := func(id string) bool { return !purged[id] }
	out := e
	out.Purges = purges
	out.Issues = keepRows(e.Issues, func(i Issue) bool { return kept(i.ID) })
	out.Relations = keepRows(e.Relations, func(r Relation) bool { return kept(r.SrcID) && kept(r.DstID) })
	out.Comments = keepRows(e.Comments, func(c Comment) bool { return kept(c.IssueID) })
	out.Labels = keepRows(e.Labels, func(l Label) bool { return kept(l.IssueID) })
	out.Events = keepRows(e.Events, func(ev IssueEvent) bool { return kept(ev.IssueID) })
	return out
}

// keepRows filters a copy, so the export ApplyPurges was handed is untouched
// and a nil collection stays nil.
func keepRows[R any](rows []R, keep func(R) bool) []R {
	return slices.DeleteFunc(slices.Clone(rows), func(row R) bool { return !keep(row) })
}
