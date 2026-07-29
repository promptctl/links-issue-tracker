package main

import (
	"strings"
	"testing"
)

func TestWriteBundle(t *testing.T) {
	entries := []Entry{
		{Module: Module{Path: "github.com/a/a", Version: "v1.0.0"}, LicenseName: "MIT", Text: "MIT text here\n"},
		{Module: Module{Path: "github.com/b/b", Version: "v2.0.0"}, LicenseName: "Unknown", Text: "some unusual license text\n"},
	}

	var buf strings.Builder
	if err := WriteBundle(&buf, entries); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	out := buf.String()

	// Order follows entries exactly — WriteBundle does no re-sorting of its
	// own, so module a must appear before module b in the output.
	aIdx := strings.Index(out, "github.com/a/a")
	bIdx := strings.Index(out, "github.com/b/b")
	if aIdx == -1 || bIdx == -1 || aIdx > bIdx {
		t.Fatalf("expected github.com/a/a before github.com/b/b in output:\n%s", out)
	}

	for _, want := range []string{
		"github.com/a/a v1.0.0", "License: MIT", "MIT text here",
		"github.com/b/b v2.0.0", "License: Unknown", "some unusual license text",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestWriteBundleDeterministic(t *testing.T) {
	entries := []Entry{
		{Module: Module{Path: "github.com/a/a", Version: "v1.0.0"}, LicenseName: "MIT", Text: "text\n"},
	}
	var first, second strings.Builder
	if err := WriteBundle(&first, entries); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if err := WriteBundle(&second, entries); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if first.String() != second.String() {
		t.Errorf("WriteBundle is not deterministic for a fixed input:\n%s\n---\n%s", first.String(), second.String())
	}
}
