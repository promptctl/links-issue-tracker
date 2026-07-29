package main

import "testing"

// TestParseModuleListAcceptReject is the accept/reject table for
// parseModuleList. The producer is linkedModuleTemplate's `go list -deps`
// output: tab-separated path/version/dir triples, one per linked package,
// with blank lines for stdlib packages the template skips. Any other shape
// is a broken producer assumption and must fail loudly rather than silently
// dropping or misreading a module. [LAW:types-are-the-program]
func TestParseModuleListAcceptReject(t *testing.T) {
	t.Run("dedupes a module linked via multiple subpackages", func(t *testing.T) {
		// golang.org/x/sys links in via many import paths, but is one module.
		in := "golang.org/x/sys\tv0.43.0\t/mod/golang.org/x/sys@v0.43.0\n" +
			"golang.org/x/sys\tv0.43.0\t/mod/golang.org/x/sys@v0.43.0\n"
		got, err := parseModuleList(in)
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d modules, want 1 (deduped): %+v", len(got), got)
		}
	})

	t.Run("sorts by module path regardless of input order", func(t *testing.T) {
		in := "zzz.example.com/z\tv1\t/mod/z\n" +
			"aaa.example.com/a\tv1\t/mod/a\n"
		got, err := parseModuleList(in)
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 2 || got[0].Path != "aaa.example.com/a" || got[1].Path != "zzz.example.com/z" {
			t.Fatalf("not sorted: %+v", got)
		}
	})

	t.Run("skips blank lines (stdlib packages)", func(t *testing.T) {
		in := "\ngithub.com/example/mod\tv1.0.0\t/mod/dir\n\n"
		got, err := parseModuleList(in)
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d modules, want 1: %+v", len(got), got)
		}
	})

	t.Run("rejects a line with the wrong field count", func(t *testing.T) {
		if _, err := parseModuleList("github.com/example/mod\tv1.0.0\n"); err == nil {
			t.Fatal("want error for a 2-field line, got nil")
		}
	})

	t.Run("rejects a record with an empty path", func(t *testing.T) {
		if _, err := parseModuleList("\tv1.0.0\t/mod/dir\n"); err == nil {
			t.Fatal("want error for an empty module path, got nil")
		}
	})

	t.Run("rejects a record with an empty dir", func(t *testing.T) {
		if _, err := parseModuleList("github.com/example/mod\tv1.0.0\t\n"); err == nil {
			t.Fatal("want error for an empty module dir, got nil")
		}
	})

	t.Run("accepts an empty-version module (local replace shape)", func(t *testing.T) {
		got, err := parseModuleList("github.com/example/mod\t\t/mod/dir\n")
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 1 || got[0].Version != "" {
			t.Fatalf("got %+v, want one module with empty version", got)
		}
	})
}
