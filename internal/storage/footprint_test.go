package storage

import "testing"

func TestFootprintOverCountsOnlyGovernedParts(t *testing.T) {
	t.Parallel()
	parts := func(chunkStore, snapshots, cache int64) []FootprintComponent {
		return []FootprintComponent{
			{Name: "chunk store", Bytes: chunkStore, Governed: true},
			{Name: "snapshots", Bytes: snapshots, Governed: true},
			{Name: "remote cache", Bytes: cache, Governed: false},
		}
	}
	cases := []struct {
		name        string
		footprint   Footprint
		wantOver    bool
		wantExcess  int64
		wantLargest string
	}{
		{
			name:      "an ungoverned part never pushes a store over",
			footprint: Footprint{Ceiling: 100, Components: parts(40, 40, 1000)},
		},
		{
			name:      "exactly at the ceiling is within it",
			footprint: Footprint{Ceiling: 100, Components: parts(60, 40, 0)},
		},
		{
			name:        "one byte over names the largest governed part",
			footprint:   Footprint{Ceiling: 100, Components: parts(30, 71, 5000)},
			wantOver:    true,
			wantExcess:  1,
			wantLargest: "snapshots",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			over, ok := tc.footprint.Over()
			if ok != tc.wantOver {
				t.Fatalf("Over() over = %v, want %v (governed %d, ceiling %d)", ok, tc.wantOver, tc.footprint.GovernedBytes(), tc.footprint.Ceiling)
			}
			if over.Excess != tc.wantExcess || over.Largest.Name != tc.wantLargest {
				t.Fatalf("Over() = %+v, want excess %d naming %q", over, tc.wantExcess, tc.wantLargest)
			}
		})
	}
}

func TestFootprintMeasuredIsTheCeiling(t *testing.T) {
	t.Parallel()
	if (Footprint{}).Measured() {
		t.Fatal("the zero Footprint reads as measured; a probe whose measurement failed would report an empty store")
	}
	if !(Footprint{Ceiling: 1}).Measured() {
		t.Fatal("a footprint carrying a ceiling reads as unmeasured")
	}
}
