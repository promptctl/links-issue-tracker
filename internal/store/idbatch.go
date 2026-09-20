package store

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
// Those are the four on the workable gather's path: the issue lookup, both
// relation endpoint queries, the label load under hydrateIssues, and the
// lifecycle children query.
//
// Not every id-keyed `IN` list can follow, and the condition is narrower than
// having one. Batching is sound only where the query's answer is the
// concatenation of its batches' answers. A `NOT IN` exclusion inverts under
// splitting -- each batch returns the very rows the others meant to exclude --
// which rules out both of ranking.go's frame queries. An `IN` that is one
// clause among several under an EXISTS, or under an `ORDER BY ... LIMIT`,
// answers a question its batches cannot be recombined into, which rules out
// ListIssues' id and parent filters and the rank lookup. requireIssues is the
// one that would batch cleanly and does not; it is a validation lookup off
// this path.
//
// Only the gather's path was audited, and nothing fails the build when a new
// unbounded id list is added. Both are recorded on links-perf-kw6z.2.
const idBatchSize = 16

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
func idBatches(ids []string) [][]string {
	unique := dedupeStrings(ids)
	if len(unique) == 0 {
		return nil
	}
	batches := make([][]string, 0, (len(unique)+idBatchSize-1)/idBatchSize)
	for start := 0; start < len(unique); start += idBatchSize {
		batches = append(batches, unique[start:min(start+idBatchSize, len(unique))])
	}
	return batches
}
