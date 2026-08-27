package memory

import (
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The optional-value comparisons and renderings below share one contract: two
// absent values are equal, absence renders as the empty string, and absence is
// never conflated with a present zero. History rows are how a caller learns
// what a mutation did, so "there was no resolution" and "the resolution was
// empty" have to stay two different readings.

func timesEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func resolutionsEqual(a, b *model.Resolution) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func stringsEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func formatTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}

func formatResolution(value *model.Resolution) string {
	if value == nil {
		return ""
	}
	return string(*value)
}

func formatString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
