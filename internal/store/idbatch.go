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
// [LAW:one-source-of-truth] Every id-keyed batch read shares this number, so
// two call sites cannot drift into different ideas of what "too many" means.
const idBatchSize = 64

// idBatches splits ids into consecutive batches of at most idBatchSize,
// preserving order.
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
	if len(ids) == 0 {
		return nil
	}
	batches := make([][]string, 0, (len(ids)+idBatchSize-1)/idBatchSize)
	for start := 0; start < len(ids); start += idBatchSize {
		batches = append(batches, ids[start:min(start+idBatchSize, len(ids))])
	}
	return batches
}
