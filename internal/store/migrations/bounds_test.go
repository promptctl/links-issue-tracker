package migrations

import (
	"strings"
	"testing"
)

// TestParseVersionShape pins the accept/reject table for ParseVersion. The
// goose producer is the source of truth: `<digits>_<name>.sql`. Anything else
// must reject — the loose `strings.Contains` shape would let unrelated names
// past, which is the enumeration-gap failure shape.
func TestParseVersionShape(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"00001_baseline.sql", 1, true},
		{"00042_add_foo.sql", 42, true},
		{"00002_x.sql", 2, true},
		{"path/to/00003_nested.sql", 3, true}, // filepath.Base strips the dir
		{"_no_digits.sql", 0, false},          // missing leading digits
		{"baseline.sql", 0, false},            // no underscore at all
		{"abc_baseline.sql", 0, false},        // non-numeric prefix
		{"00001.sql", 0, false},               // no underscore (idx <= 0)
		{"00001a_foo.sql", 0, false},          // digits + trailing letter pre-_: Sscanf("%d") would accept this (returns 1)
		{"-1_foo.sql", 0, false},              // leading sign: Sscanf("%d") would accept this (returns -1)
		{" 1_foo.sql", 0, false},              // leading whitespace: Sscanf strips it; we don't accept it
		{"+1_foo.sql", 0, false},              // explicit + sign
	}
	for _, tc := range cases {
		got, ok := ParseVersion(tc.in)
		if ok != tc.ok {
			t.Errorf("ParseVersion(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("ParseVersion(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestMaxVersionReflectsEmbeddedRegistry pins that MaxVersion's result is
// exactly the highest version in the embedded FS — it scans the FS to compute
// the expected value, so the test stays correct as new migrations land. The
// contract being pinned is "MaxVersion agrees with a fresh scan of FS and is
// at least Baseline" — NOT a hard-coded value.
func TestMaxVersionReflectsEmbeddedRegistry(t *testing.T) {
	max, err := MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion() error = %v", err)
	}
	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatalf("FS.ReadDir error = %v", err)
	}
	var expected int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, ok := ParseVersion(e.Name())
		if !ok {
			t.Fatalf("registry contains non-parseable filename %q", e.Name())
		}
		if v > expected {
			expected = v
		}
	}
	if max != expected {
		t.Fatalf("MaxVersion() = %d, but scanning FS yields %d", max, expected)
	}
	if max < Baseline {
		t.Fatalf("MaxVersion() = %d, must be >= Baseline (%d)", max, Baseline)
	}
}

// TestBaselineFileNameMatches pins that BaselineFileName resolves to a file
// that ParseVersion classifies as version Baseline. Round-trip via the parser
// so the file picker and the version parser cannot disagree.
func TestBaselineFileNameMatches(t *testing.T) {
	name, err := BaselineFileName()
	if err != nil {
		t.Fatalf("BaselineFileName() error = %v", err)
	}
	v, ok := ParseVersion(name)
	if !ok {
		t.Fatalf("BaselineFileName() returned %q which ParseVersion does not accept", name)
	}
	if v != Baseline {
		t.Fatalf("BaselineFileName() returned %q (version %d), want version %d", name, v, Baseline)
	}
	if _, err := FS.ReadFile(name); err != nil {
		t.Fatalf("BaselineFileName() returned %q but FS.ReadFile failed: %v", name, err)
	}
}
