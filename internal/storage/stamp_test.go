package storage

import (
	"testing"
	"time"
)

// A stamp is in UTC whichever input set it: the clock's reading when it is
// past history, or the nanosecond after history's newest instant when it is
// not, though both arrive spelled in other zones. Only the second counts as a
// correction, by how far history stepped the stamp past the clock.
func TestAStampIsInUTCWhicheverInputSetIt(t *testing.T) {
	at := time.Date(2026, 5, 6, 7, 8, 9, 10, time.UTC)
	west := time.FixedZone("UTC-7", -7*60*60)
	east := time.FixedZone("UTC+3", 3*60*60)
	for _, tc := range []struct {
		name            string
		clock           time.Time
		newest          time.Time
		want            time.Time
		wantCorrections StampCorrections
	}{
		{"the clock is past history", at.In(west), at.Add(-time.Hour).In(east), at, StampCorrections{}},
		{"history is past the clock", at.Add(-time.Minute).In(west), at.In(east), at.Add(time.Nanosecond), StampCorrections{Count: 1, Max: time.Minute + time.Nanosecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tally StampTally
			got := tally.Stamp(func() time.Time { return tc.clock }, tc.newest)
			if !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Fatalf("Stamp = %v, want %v in UTC", got, tc.want)
			}
			if corrections := tally.StampCorrections(); corrections != tc.wantCorrections {
				t.Fatalf("StampCorrections = %+v, want %+v", corrections, tc.wantCorrections)
			}
		})
	}
}
