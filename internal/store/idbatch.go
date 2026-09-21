package store

import "strings"

// idBatchSize caps how many ids one `IN (...)` list may carry.
//
// The engine turns each element of an IN list into an index range and merges
// that set pairwise before it reads a single row, so one clause of N ids costs
// O(N^2) range comparisons. Measured over the workable gather, 5x the ids cost
// 17x the time, with the profile sitting in compareRangeCuts and
// MySQLRangeColumnExpr.Overlaps rather than in any row scan — the cost is in
// planning the read, not performing it, which is why it hid behind queries that
// look like single batched lookups and are.
//
// Splitting the same ids into fixed batches makes the total O(N*K) for a
// constant K, which is linear in N, bought with one round trip per batch.
//
// The two terms pull opposite ways, so the total is N*fixed/K + c*N*K: a curve
// with a real minimum, not a threshold past which things are fine.
//
// Measured on the 590-row gather — nine values of K, five rounds, the order
// reshuffled every round so drift in machine load cannot alias onto K, and the
// per-K minimum taken because contention only ever adds time. Fitting
// B + A/K + C*K to those minima puts the optimum at K=13.6, with a worst
// residual of 5.2ms across values spanning 121ms to 595ms:
//
//	K      8     16     20     24     32     64    128    256    512
//	ms 130.8  121.5  122.6  125.1  130.9  162.8  228.7  353.1  594.7
//
// 16 is the measured best; 8 through 32 all sit within 8% of it, and outside
// that the right arm climbs fast — +34% at 64, +88% at 128, +389% at 512.
//
// 16 rather than the fitted 13.6, for three reasons that agree. It is the
// value actually measured lowest. A power of two folds (len(ids)+K-1)/K into
// a shift, where 20 or 24 emits a division. And it sits just above the
// optimum rather than below, which is the side to err on: every id-keyed read
// that starts batching later adds to A, and K* = sqrt(A/C) moves up with it.
//
// 13.6 is in fact a floor on the true optimum rather than an estimate of it,
// because the fixture holds one epic. lifecycleChildrenByEpicIDs batches epic
// ids, so it saw N=1 — one batch at every K — and contributed nothing to
// either term. Its query is the most expensive of the four per round trip, a
// three-way join with a compound WHERE and a two-column ORDER BY, so a real
// backlog with many epics adds to A more than to C and moves K* up again.
// Being above 13.6 is what makes that harmless.
//
// Two caveats on the numbers themselves. They are one machine, which is why
// the estimator is the minimum and why the spreads matter more than the
// levels: individual samples ran up to 1.62x apart under load, while the fit
// over the per-K minima holds to a 5.2ms worst residual across values
// spanning 121ms to 595ms. And the sweep makes this a var to set K per round,
// so it measures a division where the const folds to a shift. Both move the
// level, not the location, and it is the location that chose 16.
//
// [LAW:one-source-of-truth] The id-keyed reads that batch share this number, so
// two call sites cannot drift into different ideas of what "too many" means.
// Those are the reads on the workable gather's path — the issue lookup, both
// relation endpoint queries, the label load under hydrateIssues and the
// lifecycle children query — plus the three the sweep below added.
//
// THE RULE, for whoever writes the next id-keyed read: an `IN (...)` list may
// carry a number of elements bounded by a constant, and there are exactly two
// ways to be bounded. Either the values come from a closed domain the caller
// cannot enlarge — the five issue types, the two structural relation types —
// in which case say which domain, beside the query, because the bound is not
// visible in the clause. Or the list is caller-supplied and open, in which case
// it goes through idBatches and the query runs once per batch.
//
// Batching is sound only where the query's answer is the concatenation of its
// batches' answers, and two shapes fail that test:
//
//   - A `NOT IN` exclusion inverts under splitting: each batch returns the very
//     rows the others meant to exclude. Both of ranking.go's frame queries were
//     that shape. Neither batches, and neither needs to — the exclusion left
//     SQL entirely (see nearestRankOutside), which is the better answer wherever
//     it is available, because it adds no round trips at all.
//   - An `ORDER BY ... LIMIT` decided in SQL answers a question about the whole
//     set, which no batch holds. ListIssues is safe from this by construction
//     and not by luck: it carries no SQL ORDER BY and no SQL LIMIT, because the
//     comparator reads hydrated values the query never sees. Its sort and cap
//     run in Go over the union of the batches.
//
// The two filters that made ListIssues look unbatchable were read wrong once
// already, and the correction is worth stating: an `IN` under an EXISTS
// subquery recombines fine, because existence over a union is the union of the
// existences. ListIssues collapses its parent filter onto its id filter anyway
// (childIDsOfParents), so it batches over one id set rather than the product of
// two.
//
// Swept 2026-09-20 for links-perf-kw6z.2: every `IN (...)` in internal/store is
// now one of the two bounded kinds, and each closed-domain one names its domain
// where it is written. Nothing fails the build when a new unbounded id list is
// added, and deliberately so — the ticket asks for the latency budget to catch
// this class, not a pattern-matcher over SQL text that would be a second, drifting
// copy of the rule above.
const idBatchSize = 16

// idBatch is an id list short enough to be safe as one `IN (...)` clause.
//
// What the type buys is PROVENANCE, and it is worth being exact about that,
// because the overclaim is the interesting mistake. Go will convert any
// []string to an idBatch for free and will not check its length, so the type
// enforces no bound and the compiler proves nothing here. What it does is make
// the only way to OBTAIN one — short of writing the conversion, which is
// visible — a call to idBatches, so a reader at a query site can see that the
// count is capped without following the slice back to its caller.
//
// The rendering is where the single enforcer actually is, and it is
// repeatPlaceholder: every `IN (...)` placeholder list in this package comes
// from that one function, so grepping `IN (` against it audits the whole of the
// shape this file governs. inList is its id-shaped wrapper, pairing the
// placeholders with the args so the count of each is one fact rather than two.
// A query built from inList carries at most idBatchSize ids; a query built from
// repeatPlaceholder directly carries whatever its clause's own bound allows, and
// each of those says which vocabulary bounds it.
//
// Be exact about the scope of that claim, because the wider one is tempting and
// false. It is NOT that every `?`-list in package store comes from
// repeatPlaceholder: two do not. buildProcedureCall renders a `CALL proc(?,?)`
// argument list and legacyInsertStatement an `INSERT ... VALUES (?,?)` list, each
// counted by an arity a caller cannot enlarge — a procedure's parameters, a
// table's columns — and neither is a set-membership test. A grep for `IN (` also
// lands on the CHECK constraints in schema_reconcile.go, which carry quoted
// literals and no placeholders at all; those spell a closed vocabulary into DDL,
// and quotedIssueTypeList derives them from model.IssueTypes so the schema cannot
// drift from the sealed type. Both exceptions are named here rather than left to
// the reader's search, because a single-enforcer claim is worth only as much as
// the reader's ability to falsify it.
//
// inList is defined on a NON-EMPTY batch — idBatches never yields an empty one
// — because `IN ()` is a syntax error rather than a filter matching nothing.
// [LAW:types-are-the-program]
type idBatch []string

// inList renders the batch as the body of an `IN (...)` clause — the
// placeholders alone, without the parentheses — and the args that fill it.
//
// Returning both together is what keeps them in step: the count of `?` and the
// count of args are one fact, and the hand-rolled loops this replaces each held
// it twice.
func (b idBatch) inList() (string, []any) {
	args := make([]any, len(b))
	for i, id := range b {
		args[i] = id
	}
	return strings.Join(repeatPlaceholder(len(b)), ", "), args
}

// idBatches splits ids into consecutive batches of at most idBatchSize,
// preserving the order of their first appearance and dropping repeats.
//
// The dedupe is what keeps batching behaviour-preserving, and it belongs here
// rather than in each caller. `IN (a, ..., a)` answers once however many times
// a is written, so the single clause this replaces was indifferent to repeats;
// batches are not, and two copies of one id falling either side of a boundary
// come back as two rows. Downstream that is not an error anywhere — labels
// accumulate into a shared map and would list the same label twice, and a
// container would compose a doubled child list and report one child of two
// done. Deduping here means no caller can be written that has that bug.
// [LAW:parse-dont-validate]
//
// An empty input yields no batches rather than one empty batch, so a caller's
// loop body never runs on nothing and no caller needs its own emptiness guard
// to avoid building `IN ()`. [LAW:no-defensive-null-guards]
//
// Splitting one statement into several gives up whatever consistency the single
// statement had: a writer landing between batch k and k+1 leaves the result
// carrying some subjects as they stood before the write and others as they
// stood after, and nothing errors. That window is widened here rather than
// opened. The reads this serves were already several statements — the relation
// gather runs a src_id query, a dst_id query, and a separate issue lookup, with
// no transaction over them — so no caller had a snapshot to lose. Restoring one
// means a read transaction spanning all three, not the batch loop alone, which
// is why it is not attempted here. Tracked as links-scale-6iiv.
//
// ListIssues is the case worth stating separately, because it looks like a
// counterexample and is not quite one. Its row scan WAS a single statement, and
// under an id filter it is now one per batch, so which rows match is no longer
// decided at one instant. But its RESULT was never a snapshot: hydrateIssues
// runs the label load and the lifecycle-children query afterwards, outside any
// transaction, so a write landing between the scan and the hydration already
// produced a row carrying its own old identity and its new labels. The scan was
// the last atomic thing in it, and what it bounded was membership, not content.
// Same ticket, same fix — one read transaction over the whole call, not a
// narrower batch loop.
func idBatches(ids []string) []idBatch {
	unique := dedupeStrings(ids)
	if len(unique) == 0 {
		return nil
	}
	batches := make([]idBatch, 0, (len(unique)+idBatchSize-1)/idBatchSize)
	for start := 0; start < len(unique); start += idBatchSize {
		batches = append(batches, unique[start:min(start+idBatchSize, len(unique))])
	}
	return batches
}
