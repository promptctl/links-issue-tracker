package main

import (
	"os"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

func TestMaxSchemaAgreesWithTheEmbeddedRegistry(t *testing.T) {
	entries, err := os.ReadDir("../../" + registryInTree)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, registryInTree+e.Name())
	}
	got, err := maxSchema(names)
	if err != nil {
		t.Fatal(err)
	}
	want, err := migrations.MaxVersion()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("maxSchema over the registry directory = %d, migrations.MaxVersion() = %d", got, want)
	}
}

func TestMaxSchemaShape(t *testing.T) {
	for _, tc := range []struct {
		names []string
		want  int64
		err   string
	}{
		{names: []string{"m/00002_b.sql", "m/00010_c.sql", "m/00001_a.sql"}, want: 10},
		{names: []string{"m/00001_a.sql", "m/README.md", "m/bounds.go"}, want: 1},
		{names: []string{"m/00000_a.sql"}, want: 0},
		{names: []string{"m/00001_a.sql", "m/notes.sql"}, err: `"m/notes.sql" does not begin with a numeric version`},
		{names: []string{"m/README.md"}, err: "registry is empty"},
		{names: nil, err: "registry is empty"},
	} {
		got, err := maxSchema(tc.names)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("maxSchema(%q) err = %v, want one containing %q", tc.names, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("maxSchema(%q) = %d, %v; want %d", tc.names, got, err, tc.want)
		}
	}
}
