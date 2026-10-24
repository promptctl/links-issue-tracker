// Package designstore is the store lit's size ceiling is derived to hold: how
// many rows it has, what it was measured to cost on disk, and how far each
// measured figure may grow before the ceiling's derivation stops being true.
//
// It is a leaf with no imports so that both readers take the same constants
// and nothing else: internal/store derives storeSizeCeiling from the
// allowances, and tools/perfbench judges a fresh design-size measurement
// against them without linking the engine. [LAW:one-source-of-truth]
package designstore

// Rows is the design size, fixed on 2026-10-21. Nothing in this tree derives
// it: the measured figures below were taken at it, so it moves only with them.
// The real stores' sizes are tools/perfbench's fleetCensus, counted on
// 2026-10-23 by a stated rule.
const Rows = 2_800

// The measured figures are the design size's column of `just perf --sizes
// design`, taken 2026-10-23 on darwin/arm64; re-run it to check them. They are
// measured at the design size rather than scaled from a smaller store because
// neither scales linearly: a row cost 35 KB at 590 rows and 67 KB at 2,800, a
// commit 18 KB and 30 KB.
const (
	// measuredCompactedBytes is the design-size store's compacted bytes.
	measuredCompactedBytes int64 = 186_880_000
	// measuredCommitBytes is the design-size store's bytes/commit.
	measuredCommitBytes int64 = 29_850
)

// HeadroomPercent is how far past its measurement each figure may grow while
// the ceiling still holds a design-size store. Three design-size runs on
// 2026-10-23 spread 2.6% per store (184.36-189.15 MB) and 3.9% per commit
// (28.73-29.85 KB), and the figures above sit 1.2% and 11.3% over the
// 2026-10-21 ones (184.61 MB, 26.82 KB). 25% clears that noise several times
// over and that drift twice, and a store a quarter heavier per row or per
// commit is a regression worth a ticket. A figure past its allowance is
// answered by that ticket, never by raising the allowance, and re-measuring
// into the figures above raises it as surely as raising this does: either
// turns the nightly lane green with the alarm disconnected. The figures move
// only with the ticket that found why a design-size store's cost moved.
const HeadroomPercent = 25

// The allowances are the figures the ceiling is derived from, and the most a
// fresh design-size measurement may report. A measurement within both is a
// store the ceiling holds; one past either makes the ceiling's derivation
// false.
const (
	CompactedBytesAllowance = measuredCompactedBytes * (100 + HeadroomPercent) / 100
	CommitBytesAllowance    = measuredCommitBytes * (100 + HeadroomPercent) / 100
)
