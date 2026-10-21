package storage

import "fmt"

// Footprint is a store's size on disk, part by part, beside the ceiling the
// engine holds it under. The engine names the parts, because what a store is
// made of on disk is the engine's to know; whether the parts exceed the
// ceiling is decided here, once, for every engine and every reader — the
// backstop's trail and doctor read the same answer. [LAW:single-enforcer]
type Footprint struct {
	Components []FootprintComponent `json:"components"`
	// Ceiling is the bound the governed components are held under, in
	// apparent bytes.
	Ceiling int64 `json:"ceiling"`
}

// FootprintComponent is one part of a store on disk.
type FootprintComponent struct {
	Name string `json:"name"`
	// Bytes is the part's apparent size: the sum of its files' lengths, the
	// same number on every filesystem whatever its block size.
	Bytes int64 `json:"bytes"`
	// Governed is whether the ceiling counts this part. A part outside it is
	// still measured and reported, because a reader asking where the disk
	// went is owed every part; the engine says why one is left out.
	Governed bool `json:"governed"`
}

// Overage is how far a footprint stands over its ceiling, and the part a
// reader should look at first.
type Overage struct {
	Excess int64
	// Largest is the governed part holding the most bytes. When nothing is
	// left to reclaim, it is the part that has to shrink for the store to
	// come back under.
	Largest FootprintComponent
}

// Measured reports whether this footprint was read from a store. An engine's
// ceiling is never zero, so the zero Footprint is the one no measurement
// produced, and a reader can tell "measured and empty" from "never measured"
// without a companion flag. [LAW:types-are-the-program]
func (f Footprint) Measured() bool { return f.Ceiling > 0 }

// GovernedBytes totals the parts the ceiling counts.
func (f Footprint) GovernedBytes() int64 {
	var total int64
	for _, c := range f.Components {
		if c.Governed {
			total += c.Bytes
		}
	}
	return total
}

// Over reports the overage when the governed parts exceed the ceiling. A
// footprint exactly at its ceiling is within it.
func (f Footprint) Over() (Overage, bool) {
	excess := f.GovernedBytes() - f.Ceiling
	if excess <= 0 {
		return Overage{}, false
	}
	var largest FootprintComponent
	for _, c := range f.Components {
		if c.Governed && c.Bytes > largest.Bytes {
			largest = c
		}
	}
	return Overage{Excess: excess, Largest: largest}, true
}

// FormatBytes renders a byte count for operator-facing output, in binary
// units. Every size lit shows a person — a compaction's reclaim, a cache
// collection's, a footprint and its ceiling — is spelled here, so no two of
// them can disagree about how a size reads. [LAW:one-source-of-truth]
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
