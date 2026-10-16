package migrations

import (
	"os"
	"slices"
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
		{"00000_foo.sql", 0, false},           // version 0: goose refuses versions below 1
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

// TestParseRegistryShape pins the registry rule over a directory's file names:
// the *.sql files are the migrations, each must begin with a version, no two
// share one, there is at least one, and they come back in ascending version
// order whatever order the names arrive in.
func TestParseRegistryShape(t *testing.T) {
	for _, tc := range []struct {
		names []string
		want  []Migration
		err   string
	}{
		{names: []string{"00002_b.sql", "00010_c.sql", "00001_a.sql"},
			want: []Migration{{1, "00001_a.sql"}, {2, "00002_b.sql"}, {10, "00010_c.sql"}}},
		{names: []string{"00001_a.sql", "README.md", "bounds.go"}, want: []Migration{{1, "00001_a.sql"}}},
		{names: []string{"00001_a.sql", "00000_z.sql"}, err: `"00000_z.sql" does not begin with a version`},
		{names: []string{"00001_a.sql", "notes.sql"}, err: `"notes.sql" does not begin with a version`},
		{names: []string{"00002_b.sql", "00001_a.sql", "00002_a.sql"}, err: `version 2 is claimed by both "00002_a.sql" and "00002_b.sql"`},
		{names: []string{"README.md"}, err: "registry is empty"},
		{names: nil, err: "registry is empty"},
	} {
		got, err := ParseRegistry(tc.names)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ParseRegistry(%q) err = %v, want one containing %q", tc.names, err, tc.err)
			}
			continue
		}
		if err != nil || !slices.Equal(got.Migrations(), tc.want) {
			t.Errorf("ParseRegistry(%q) = %v, %v; want %v", tc.names, got.Migrations(), err, tc.want)
			continue
		}
		if max := got.MaxVersion(); max != tc.want[len(tc.want)-1].Version {
			t.Errorf("ParseRegistry(%q).MaxVersion() = %d, want %d", tc.names, max, tc.want[len(tc.want)-1].Version)
		}
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

// TestEmbedHoldsTheRegistryDirectory pins FS to the directory it embeds from.
// schema-release-check reads a release tag's registry as git lists this
// directory and this tree's from FS, so the two must parse to the same
// migrations, or the two sides of its comparison read different registries.
func TestEmbedHoldsTheRegistryDirectory(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	dir, err := ParseRegistry(names)
	if err != nil {
		t.Fatalf("the registry directory: %v", err)
	}
	embedded, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dir.Migrations(), embedded.Migrations()) {
		t.Errorf("the registry directory holds %v; FS embeds %v", dir.Migrations(), embedded.Migrations())
	}
}
