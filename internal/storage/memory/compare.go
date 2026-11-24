package memory

import "time"

// The optional-value comparisons and renderings below share one contract: two
// absent values are equal, absence renders as the empty string, and absence is
// never conflated with a present zero. History rows are how a caller learns
// what a mutation did, so "there was no archive time" and "the archive time was
// the zero instant" have to stay two different readings.

func timesEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
