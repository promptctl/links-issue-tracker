package storage_test

import (
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A write's stamp is in UTC whichever instant it follows: the clock's reading,
// or history ahead of the clock read back with an offset, which the stamp
// steps one nanosecond past.
func TestStampIsUTCOnEveryBranch(t *testing.T) {
	offset := time.FixedZone("UTC-5", -5*60*60)
	for _, tc := range []struct {
		name   string
		clock  time.Time
		newest time.Time
		want   time.Time
	}{
		{"clock ahead of history", born.In(offset), born.Add(-time.Hour), born},
		{"history ahead of clock", born, born.Add(time.Hour).In(offset), born.Add(time.Hour + time.Nanosecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tally storage.StampTally
			got := tally.Stamp(func() time.Time { return tc.clock }, tc.newest)
			if !got.Equal(tc.want) {
				t.Fatalf("Stamp() = %v, want %v", got, tc.want)
			}
			if got.Location() != time.UTC {
				t.Fatalf("Stamp() = %v in %v, want UTC", got, got.Location())
			}
		})
	}
}
