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
// On the value of K, only the upper end is measured. Sweeping the 590-row
// gather, 256 and 512 are clearly worse than the rest (1.17s and 1.45s against
// ~0.96s in the same run), which is the quadratic term still being felt. Every
// value from 16 to 128 came out within the noise of the machine, and no
// instrument here separated them — so 64 is a mid-range pick inside the flat
// region rather than a tuned optimum, and anyone re-tuning it should get a
// quiet machine first and expect to be choosing between roughly equal options.
//
// [LAW:one-source-of-truth] The id-keyed reads that batch share this number, so
// two call sites cannot drift into different ideas of what "too many" means.
// Those are the four on the workable gather's path: the issue lookup, both
// relation endpoint queries, the label load under hydrateIssues, and the
// lifecycle children query.
//
// Two id-keyed `IN` lists are deliberately left unbatched, and neither is a
// loop away from it. ListIssues takes its id filter as one clause among several
// in a query carrying its own ordering and limit, and the rank query's list
// feeds an `ORDER BY ... LIMIT 1` whose answer is not the concatenation of its
// batches' answers. Splitting either changes what the query means rather than
// how many round trips it takes, so both want their own reasoning and are
// recorded on links-perf-kw6z.2 instead of being swept in here.
const idBatchSize = 64

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
