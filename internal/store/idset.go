package store

import (
	"encoding/json"
	"strings"
)

// idSet is a caller-supplied list of issue ids, deduplicated, rendered as one
// row source a query joins against: a JSON_TABLE over a single JSON-array
// parameter. Every id-keyed read in this package takes one, so a read costs
// one query however many ids it carries.
//
// Why a join and not the obvious `id IN (?, ?, ...)`: the engine turns each
// element of an IN list into an index range and merges the ranges pairwise
// before it reads a row, so one clause of N ids costs O(N^2) in planning. The
// previous answer was to split the ids into batches of 16, which made the
// planning linear but paid a full planner pass per batch — at 2,800 issues the
// label read alone was 182 queries, and planning was 46% of `lit backlog`'s
// CPU. A join over a JSON_TABLE is planned once, as a join, with no ranges to
// merge. Measured on a 2,800-issue store:
//
//	                      16-id batches   one idSet join
//	labels of every id        50-97ms          0.3-0.8ms
//	children of every id        183ms             43ms
//
// THE TWO SHAPES THE PLANNER PUNISHES, for whoever writes the next id-keyed
// read — both look like the same query and neither errors:
//
//  1. `x IN (?, ?, ...)` over a list a caller can lengthen. Quadratic planning,
//     as above. Use an idSet.
//  2. `x IN (SELECT id FROM JSON_TABLE(...))` — the idSet written as a
//     subquery rather than joined. On the three-way children join it plans
//     quadratically in the rows instead: 5s against the join's 43ms. So the
//     set is only ever a join source, and rows is its only rendering.
//
// An `IN (...)` list remains right where its length is bounded by something
// other than the caller: a CLOSED DOMAIN (the five issue types, the two
// structural relation types — say which beside the clause), or PROVENANCE
// (ListIssues' assignee clause, whose values a person types and nothing derives
// from the backlog — say so beside the clause, because that bound is invisible
// in the SQL and a caller deriving the list from the backlog breaks it
// silently). placeholders renders those, and only those.
//
// One query rather than several also retires the soundness rules batching
// needed — a NOT IN that inverted under splitting, an ORDER BY ... LIMIT no
// batch could answer — because nothing is split.
type idSet struct {
	ids []string
}

// newIDSet dedupes ids, keeping the order of first appearance.
//
// The dedupe is what keeps the join behaviour-preserving: `IN (a, a)` matches
// a row once, but a join against a set holding a twice returns the row twice,
// and downstream a doubled row is no error anywhere — labels accumulate into a
// shared map and would list a label twice, and a container would compose a
// doubled child list. Deduping here means no caller can be written that has
// that bug. [LAW:parse-dont-validate]
//
// An empty set is a legal value and needs no guard at any caller: a join
// against no rows returns no rows, where `IN ()` was a syntax error.
// [LAW:no-defensive-null-guards]
func newIDSet(ids []string) idSet {
	return idSet{ids: dedupeStrings(ids)}
}

// rows renders the set as a FROM-clause row source named alias, with one
// column, id, typed as the schema types every id column, and the one argument
// that binds it.
//
// The rendering is the single enforcer of the shape above: a query that joins
// rows cannot have written the set as an IN list or a subquery.
// [LAW:single-enforcer]
func (s idSet) rows(alias string) (string, any) {
	// A []string always marshals, and dedupeStrings never returns nil, so the
	// empty set encodes as [] (no rows) rather than null.
	encoded, err := json.Marshal(s.ids)
	if err != nil {
		panic("store: marshal an id list: " + err.Error())
	}
	return `JSON_TABLE(?, '$[*]' COLUMNS (id VARCHAR(191) PATH '$')) AS ` + alias, string(encoded)
}

// idChunks splits ids into consecutive runs of at most idChunkSize, keeping
// the order of first appearance and dropping repeats. It serves the one
// statement in this package that still has to bound a list by length rather
// than join a set: writeRanksTx's CASE update, because Dolt evaluates an
// UPDATE ... JOIN's check constraints against the joined row and refuses valid
// writes. Reads never chunk; they join an idSet.
func idChunks(ids []string) [][]string {
	unique := dedupeStrings(ids)
	chunks := make([][]string, 0, (len(unique)+idChunkSize-1)/idChunkSize)
	for start := 0; start < len(unique); start += idChunkSize {
		chunks = append(chunks, unique[start:min(start+idChunkSize, len(unique))])
	}
	return chunks
}

// idChunkSize bounds the ids one CASE update carries. 16 was measured as the
// best IN-list batch for reads, where the O(N^2) range merge and the per-query
// planning pull opposite ways; a write's IN list has the same planner.
const idChunkSize = 16

// placeholders renders n `?` tokens as the body of an IN list.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
