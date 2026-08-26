package store

import (
	"database/sql"
	"testing"
)

// TestParseUnixSeconds pins how the driver's UNIX_TIMESTAMP rendering is decoded:
// NULL and empty map to absent, a fractional decimal truncates to whole seconds,
// and unparseable output is a loud error — never a silent zero that would date a
// divergence to 1970. The fractional-decimal case is the real driver format that
// forced the NullString scan in the first place. [LAW:no-silent-failure]
func TestParseUnixSeconds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		raw       sql.NullString
		wantValid bool
		wantValue int64
		wantErr   bool
	}{
		{"null (empty range)", sql.NullString{Valid: false}, false, 0, false},
		{"empty string", sql.NullString{String: "", Valid: true}, false, 0, false},
		{"whitespace only", sql.NullString{String: "   ", Valid: true}, false, 0, false},
		{"fractional decimal", sql.NullString{String: "1784998962.153", Valid: true}, true, 1784998962, false},
		{"whole seconds", sql.NullString{String: "1700000000", Valid: true}, true, 1700000000, false},
		{"truncates sub-second up", sql.NullString{String: "1700000000.999", Valid: true}, true, 1700000000, false},
		{"padded value", sql.NullString{String: "  1700000000.5 ", Valid: true}, true, 1700000000, false},
		{"unparseable is an error", sql.NullString{String: "not-a-number", Valid: true}, false, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUnixSeconds(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseUnixSeconds(%+v) err = %v, wantErr = %v", tc.raw, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got.Valid != tc.wantValid || got.Int64 != tc.wantValue {
				t.Fatalf("parseUnixSeconds(%+v) = {%d, valid=%v}, want {%d, valid=%v}", tc.raw, got.Int64, got.Valid, tc.wantValue, tc.wantValid)
			}
		})
	}
}

// TestEarlierValidUnix pins the four branches: both valid picks the smaller
// (either ordering), one-sided validity passes the valid one through, and neither
// valid is 0 (absent). This is what dates a divergence to its earliest fork commit
// across the two ranges.
func TestEarlierValidUnix(t *testing.T) {
	t.Parallel()
	v := func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	invalid := sql.NullInt64{Valid: false}
	cases := []struct {
		name string
		a, b sql.NullInt64
		want int64
	}{
		{"both valid, a earlier", v(100), v(200), 100},
		{"both valid, b earlier", v(200), v(100), 100},
		{"both valid, equal", v(150), v(150), 150},
		{"a only", v(100), invalid, 100},
		{"b only", invalid, v(200), 200},
		{"neither", invalid, invalid, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := earlierValidUnix(tc.a, tc.b); got != tc.want {
				t.Fatalf("earlierValidUnix(%+v, %+v) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
