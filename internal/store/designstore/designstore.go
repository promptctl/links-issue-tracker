// Package designstore is the store lit's size ceiling is derived to hold: how
// many rows it has, what it was measured to cost on disk, and how far each
// measured figure may grow before the ceiling's derivation stops being true.
//
// It is a leaf with no imports so that both readers take the same constants
// and nothing else: internal/store derives storeSizeCeiling from the
// allowances, and tools/perfbench judges a fresh design-size measurement
// against them without linking the engine. [LAW:one-source-of-truth]
package designstore

// Rows is the design size: a chosen figure above every real store, which
// tools/perfbench's fleetCensus counts and its tests hold Rows over. The
// measured figures below were taken at it, so it moves only with them.
const Rows = 2_800

// The measured figures are the design size's column of `just perf --sizes
// design` on darwin/arm64, compacted bytes taken 2026-11-01 and bytes/commit
// 2026-10-23; re-run it to check them. They are measured at the design size
// rather than scaled from a smaller store because neither scales linearly: on
// 2026-11-01 a row cost 26 KB at 590 rows and 41-43 KB at 2,800, a commit
// 17-18 KB and 25-28 KB.
const (
	// measuredCompactedBytes is the design-size store's compacted bytes. It is
	// a generated store's, whose rank keys are short. A store written before
	// keys stopped growing keeps the long ones, which only a respace at a
	// crowded spot rewrites, and costs more per row than this.
	measuredCompactedBytes int64 = 114_610_000
	// measuredCommitBytes is the design-size store's bytes/commit. It sits 7-20%
	// over the 2026-11-01 runs, which fell with no cause found, so a re-run
	// reads under it and the rule below keeps it.
	measuredCommitBytes int64 = 29_850
)

// HeadroomPercent is how far past its measurement each figure may grow while
// the ceiling still holds a design-size store. Five design-size runs on
// 2026-11-01 spread 4.2% per store (114.24-119.04 MB) and 12.0% per commit
// (24.80-27.77 KB). 25% clears that noise twice over, and a store a quarter
// heavier per row or per commit is a regression worth a ticket. A figure past
// its allowance is answered by that ticket, never by raising the allowance,
// and re-measuring into the figures above raises it as surely as raising this
// does: either turns the nightly lane green with the alarm disconnected. The
// figures move only with the ticket that found why a design-size store's cost
// moved.
const HeadroomPercent = 25

// The allowances are the figures the ceiling is derived from, and the most a
// fresh design-size measurement may report. A measurement within both is a
// store the ceiling holds; one past either makes the ceiling's derivation
// false.
const (
	CompactedBytesAllowance = measuredCompactedBytes * (100 + HeadroomPercent) / 100
	CommitBytesAllowance    = measuredCommitBytes * (100 + HeadroomPercent) / 100
)
