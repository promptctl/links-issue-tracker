package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The write boundary refuses every write that would leave a ticket outside an
// epic (containment.go). A write that rewrites the hierarchy from data — a sync
// landing a merged or adopted backlog, a restore landing a backup — has no
// single write to refuse: the shape it lands is whatever the data adds up to.
// A purge on one clone can drop the epic a ticket on the other clone was just
// filed under, and a backup can hold a parent purged since it was taken. So
// those writes bring the data under the rule instead, the way migration 00007
// brought the data stored before the rule existed, and say what they moved.
// [LAW:single-enforcer] one pass, called by every write that lands an export
// whole; the rule it applies is 00007's, so a ticket stranded today lands where
// a ticket stranded before v7 landed.

// Rehomed is one ticket a hierarchy rewrite found outside an epic, and the epic
// it lives in now. Epic is empty only for an epic that sat under a ticket that
// is not one and had no epic above it: it now stands at the top level, where an
// epic may live.
type Rehomed struct {
	IssueID string `json:"issue_id"`
	Epic    string `json:"epic"`
}

// The store's unfiled epic, as migration 00007 mints it. Every checkout mints
// the same row — the id is a hash of the prefix, the instant is fixed — so two
// clones that each strand a ticket hold one epic, not two tickets under one id
// (merge.Classify compares exactly the birth instant), and its text never
// diverges into a held prose merge. The migration is frozen, so these are the
// copy every later mint reads; TestUnfiledEpicMatchesMigration00007 holds them
// to the row the migration writes. [LAW:one-source-of-truth]
const (
	UnfiledTitle       = "Unfiled tickets"
	UnfiledTopic       = "unfiled"
	unfiledDescription = "Tickets that had no epic when every ticket came to live inside one (schema v7): top-level tasks, bugs, features and chores, and children of tickets that are not epics with no epic above them. Each belongs to some effort; move it there with `lit parent set --child <id> --parent <epic-id>`. Every ticket here is its own lane, as it was at the top level, so they are worked in parallel rather than in sequence. This epic is finished when it is empty."
	// unfiledFallbackPrefix names the epic of a store none of whose ids reads
	// back to a prefix.
	unfiledFallbackPrefix = "lit"
	// rehomeEdgeAuthor stamps the edge a move writes, the stamp `lit new
	// --parent` writes on the edge it creates.
	rehomeEdgeAuthor = "links"
	// climbDepthCut ends a climb through a loop of tickets that are not epics.
	// No real hierarchy is this deep.
	climbDepthCut = 64
)

var unfiledBorn = time.Date(2026, time.October, 12, 0, 0, 0, 0, time.UTC)

// UnfiledEpicID is the id migration 00007 mints for the unfiled epic of a
// store whose ids carry prefix, the first id in prefix's chain: rendered the
// way `lit new` renders a top-level id, with the hash fixed to the first four
// hex digits of SHA-256 over the prefix and the topic.
func UnfiledEpicID(prefix string) string {
	return unfiledEpicIDOver(prefix, prefix+"-"+UnfiledTopic)
}

// unfiledEpicIDOver renders an unfiled epic id under prefix with the hash
// taken over seed. Each id after the first in prefix's chain is hashed over the
// one before it: a purged id is never reoccupied, so when the unfiled epic is
// purged the next one takes the next id, and every clone holding the same
// purge records walks to the same one.
func unfiledEpicIDOver(prefix, seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return prefix + "-" + UnfiledTopic + "-" + hex.EncodeToString(sum[:])[:4]
}

// HomeStranded returns export with every ticket inside an epic, and names each
// ticket it moved, sorted by id. A ticket whose parent is not an epic moves to
// the nearest epic above it; a ticket with no epic above it — a leaf with no
// parent at all, or a child of tickets that are not epics with none over them
// — moves under the store's unfiled epic, minted here when the export does not
// hold it. An epic with no epic above it moves to the top level instead. A moved
// ticket becomes its own lane, as it was wherever it stood before, so the move
// does not queue it behind unrelated work. A ticket inside an epic stays where
// it is and only loses a second parent edge onto a ticket that is not an epic.
// An export with nothing stranded comes back unchanged with nothing named.
//
// It is pure, and every value it writes derives from the export, so two clones
// moving the same tickets write the same rows. [LAW:effects-at-boundaries]
func HomeStranded(export model.Export) (model.Export, []Rehomed) {
	h := newHierarchy(export)
	moves := map[string]string{}
	strandedLeaf := false
	for _, issue := range export.Issues {
		parents := h.parentsOf[issue.ID]
		if slices.ContainsFunc(parents, h.isEpic) {
			continue
		}
		if len(parents) == 0 && issue.IsContainer() {
			continue
		}
		epic, found := h.nearestEpicAbove(issue.ID, parents)
		moves[issue.ID] = epic
		strandedLeaf = strandedLeaf || (!found && !issue.IsContainer())
	}
	trimmed := h.childrenOfNonEpics(export.Issues, moves)
	if len(moves) == 0 && len(trimmed) == 0 {
		return export, nil
	}

	out := export
	out.Issues = slices.Clone(export.Issues)
	unfiled := ""
	if strandedLeaf {
		var minted *model.Issue
		unfiled, minted = h.unfiledEpic(export)
		if minted != nil {
			out.Issues = append(out.Issues, *minted)
		}
	}
	rehomed := make([]Rehomed, 0, len(moves)+len(trimmed))
	for i, issue := range out.Issues {
		epic, moved := moves[issue.ID]
		if !moved {
			continue
		}
		if epic == "" && !issue.IsContainer() {
			epic = unfiled
		}
		moves[issue.ID] = epic
		out.Issues[i].Lane = issue.ID
		rehomed = append(rehomed, Rehomed{IssueID: issue.ID, Epic: epic})
	}
	for _, child := range trimmed {
		rehomed = append(rehomed, Rehomed{IssueID: child, Epic: h.epicParentOf(child)})
	}

	// Every edge onto a parent that is not an epic goes — the old edge of each
	// moved ticket, and a second parent beside an epic — and each move to an
	// epic adds its one new edge, stamped with the ticket's birth.
	out.Relations = slices.DeleteFunc(slices.Clone(export.Relations), func(r model.Relation) bool {
		return r.Type == model.RelParentChild && !h.isEpic(r.DstID)
	})
	for _, issue := range out.Issues {
		if epic := moves[issue.ID]; epic != "" {
			out.Relations = append(out.Relations, model.Relation{SrcID: issue.ID, DstID: epic, Type: model.RelParentChild, CreatedAt: issue.CreatedAt, CreatedBy: rehomeEdgeAuthor})
		}
	}
	slices.SortFunc(rehomed, func(a, b Rehomed) int { return strings.Compare(a.IssueID, b.IssueID) })
	return out, rehomed
}

// hierarchy is the export's parent edges read once: each ticket's type, and
// the parents each child names.
type hierarchy struct {
	typeOf    map[string]model.IssueType
	parentsOf map[string][]string
}

func newHierarchy(export model.Export) hierarchy {
	h := hierarchy{typeOf: make(map[string]model.IssueType, len(export.Issues)), parentsOf: map[string][]string{}}
	for _, issue := range export.Issues {
		h.typeOf[issue.ID] = issue.IssueType
	}
	for _, r := range export.Relations {
		if r.Type == model.RelParentChild {
			h.parentsOf[r.SrcID] = append(h.parentsOf[r.SrcID], r.DstID)
		}
	}
	return h
}

func (h hierarchy) isEpic(id string) bool {
	t, ok := h.typeOf[id]
	return ok && t.IsContainer()
}

// nearestEpicAbove climbs from child's parents, none of them an epic, through
// each ancestor that is a ticket but not an epic, and returns the nearest epic
// any climb reaches — the lowest id among epics at that depth, the lower-id
// rule every listing applies. The climb is a set, so a fork inside a loop is
// walked once; it stops at an epic, at a ticket with no parent, at an id the
// export does not hold, and at the depth cut. An epic in a loop through a
// ticket that is not one can climb back to itself; when it is the nearest, it
// has no epic above it and stands at the top level, as 00007 leaves it.
func (h hierarchy) nearestEpicAbove(child string, parents []string) (string, bool) {
	seen := map[string]bool{}
	level := parents
	for depth := 0; depth <= climbDepthCut && len(level) > 0; depth++ {
		var epics, next []string
		for _, id := range level {
			if seen[id] {
				continue
			}
			seen[id] = true
			if h.isEpic(id) {
				epics = append(epics, id)
				continue
			}
			if _, held := h.typeOf[id]; held {
				next = append(next, h.parentsOf[id]...)
			}
		}
		if len(epics) > 0 {
			if nearest := slices.Min(epics); nearest != child {
				return nearest, true
			}
			return "", false
		}
		level = next
	}
	return "", false
}

// childrenOfNonEpics is every ticket that stays inside its epic but also holds
// an edge onto a parent that is not one, sorted. moves is excluded: a moved
// ticket loses all its edges and is named by its move. It walks the tickets,
// not the edges: an edge whose child the export does not hold names no ticket
// to move, and is left for the store's own write to refuse.
func (h hierarchy) childrenOfNonEpics(issues []model.Issue, moves map[string]string) []string {
	var out []string
	for _, issue := range issues {
		if _, moved := moves[issue.ID]; moved {
			continue
		}
		if slices.ContainsFunc(h.parentsOf[issue.ID], func(p string) bool { return !h.isEpic(p) }) {
			out = append(out, issue.ID)
		}
	}
	slices.Sort(out)
	return out
}

// epicParentOf is the lowest-id epic among child's parents; the caller asks it
// only of a ticket it knows holds one.
func (h hierarchy) epicParentOf(child string) string {
	return slices.Min(slices.DeleteFunc(slices.Clone(h.parentsOf[child]), func(p string) bool { return !h.isEpic(p) }))
}

// unfiledEpic returns the id of the store's unfiled epic, and the row to add
// when the export does not hold it yet. An epic the export holds — minted by
// the migration, received from a peer, or minted by an earlier rewrite — is
// never rewritten. It is recognized by its type, topic and title, which still
// find it once most ids carry another prefix, and by its id, which still finds
// it once it is retitled. Otherwise the epic takes the first id in the chain
// under the prefix most of the export's top-level ids carry (ties to the lower
// prefix) that the export neither holds nor has purged. An epic that is
// archived or deleted takes no ticket — one filed there would leave the flow
// with it — so neither rule picks it and the chain walks past its id, as it
// walks past a purged one. The minted epic sits where the highest-ranked live
// top-level leaf sits, one key after it, and is unranked when no such leaf has
// a key — the placement 00007 gives it.
func (h hierarchy) unfiledEpic(export model.Export) (string, *model.Issue) {
	var titled []string
	held := make(map[string]model.Issue, len(export.Issues))
	counts := map[string]int{}
	topRank := ""
	for _, issue := range export.Issues {
		held[issue.ID] = issue
		if takesTickets(issue) && issue.Topic == UnfiledTopic && issue.Title == UnfiledTitle {
			titled = append(titled, issue.ID)
		}
		if prefix, ok := issueid.PrefixOf(issue.ID, issue.Topic); ok {
			counts[prefix]++
		}
		_, deleted := issue.Retention().(model.Deleted)
		if !issue.IsContainer() && len(h.parentsOf[issue.ID]) == 0 && issue.Rank != "" && !deleted && (topRank == "" || issue.Rank < topRank) {
			topRank = issue.Rank
		}
	}
	if len(titled) > 0 {
		return slices.Min(titled), nil
	}
	purged := make(map[string]bool, len(export.Purges))
	for _, p := range export.Purges {
		purged[p.IssueID] = true
	}
	prefix, most := unfiledFallbackPrefix, 0
	for _, p := range slices.Sorted(maps.Keys(counts)) {
		if counts[p] > most {
			prefix, most = p, counts[p]
		}
	}
	rank := ""
	if topRank != "" {
		rank = topRank + "0"
	}
	// Every id the chain walks past is a row or a purge record the export
	// holds, so a free id comes within one more than their count.
	id := UnfiledEpicID(prefix)
	for range len(held) + len(purged) + 1 {
		row, occupied := held[id]
		switch {
		case occupied && takesTickets(row):
			return id, nil
		case !occupied && !purged[id]:
			return id, &model.Issue{
				ID:          id,
				Title:       UnfiledTitle,
				Description: unfiledDescription,
				IssueType:   model.TypeEpic,
				Topic:       UnfiledTopic,
				Rank:        rank,
				CreatedAt:   unfiledBorn,
				UpdatedAt:   unfiledBorn,
			}
		}
		id = unfiledEpicIDOver(prefix, id)
	}
	panic(fmt.Sprintf("unfiled epic: the id chain under %q cycled through %d held or purged ids without a free one", prefix, len(held)+len(purged)))
}

// takesTickets reports whether issue is an epic in the flow, the one kind of
// row a ticket can be filed under and stay in the flow itself.
func takesTickets(issue model.Issue) bool {
	return issue.IsContainer() && !model.Frozen(issue.Retention())
}
