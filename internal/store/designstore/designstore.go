// Package designstore is the store lit's size ceiling is derived to hold: how
// many rows it has, what it was measured to cost on disk, and how far each
// measured figure may grow before the ceiling's derivation stops being true.
//
// It is a leaf with no imports so that both readers take the same constants
// and nothing else: internal/store derives storeSizeCeiling from the
// allowances, and tools/perfbench judges a fresh design-size measurement
// against them without linking the engine. [LAW:one-source-of-truth]
package designstore

// Rows is the design size: twice this repository's 1,383 rows on 2026-10-21,
// the largest lit store known.
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
// the ceiling still holds a design-size store. Run to run the figures move
// about 3% (a 590-row store, 23.1-23.8 MB), and from 2026-10-21 to 2026-10-23
// they drifted 2.4% per store and 6.6% per commit; 25% clears both several
// times over, and a store a quarter heavier per row or per commit is a
// regression worth a ticket. A figure past its allowance is answered by that
// ticket, never by raising this: an allowance raised to turn the nightly lane
// green is the alarm disconnected.
const HeadroomPercent = 25

// The allowances are the figures the ceiling is derived from, and the most a
// fresh design-size measurement may report. A measurement within both is a
// store the ceiling holds; one past either makes the ceiling's derivation
// false.
const (
	CompactedBytesAllowance = measuredCompactedBytes * (100 + HeadroomPercent) / 100
	CommitBytesAllowance    = measuredCommitBytes * (100 + HeadroomPercent) / 100
)
