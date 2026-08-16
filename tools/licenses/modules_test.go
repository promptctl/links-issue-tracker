package main

import "testing"

// TestParseModuleListAcceptReject is the accept/reject table for
// parseModuleList. The producers are linkedModuleTemplate's `go list -deps`
// output and graphModuleTemplate's `go list -m all` output: tab-separated
// path/version/dir/replacement quads, one per module, with blank lines for the
// records both templates skip (stdlib packages, the main module). Any other
// shape is a broken producer assumption and must fail loudly rather than
// silently dropping or misreading a module. [LAW:types-are-the-program]
func TestParseModuleListAcceptReject(t *testing.T) {
	t.Run("dedupes a module linked via multiple subpackages", func(t *testing.T) {
		// golang.org/x/sys links in via many import paths, but is one module.
		in := "golang.org/x/sys\tv0.43.0\t/mod/golang.org/x/sys@v0.43.0\t\n" +
			"golang.org/x/sys\tv0.43.0\t/mod/golang.org/x/sys@v0.43.0\t\n"
		got, err := parseModuleList(in)
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d modules, want 1 (deduped): %+v", len(got), got)
		}
	})

	t.Run("sorts by module path regardless of input order", func(t *testing.T) {
		in := "zzz.example.com/z\tv1\t/mod/z\t\n" +
			"aaa.example.com/a\tv1\t/mod/a\t\n"
		got, err := parseModuleList(in)
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 2 || got[0].Path != "aaa.example.com/a" || got[1].Path != "zzz.example.com/z" {
			t.Fatalf("not sorted: %+v", got)
		}
	})

	t.Run("skips blank lines (stdlib packages)", func(t *testing.T) {
		in := "\ngithub.com/example/mod\tv1.0.0\t/mod/dir\t\n\n"
		got, err := parseModuleList(in)
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d modules, want 1: %+v", len(got), got)
		}
	})

	t.Run("rejects a line with the wrong field count", func(t *testing.T) {
		if _, err := parseModuleList("github.com/example/mod\tv1.0.0\t/mod/dir\n"); err == nil {
			t.Fatal("want error for a 3-field line, got nil")
		}
	})

	t.Run("rejects a record with an empty path", func(t *testing.T) {
		if _, err := parseModuleList("\tv1.0.0\t/mod/dir\t\n"); err == nil {
			t.Fatal("want error for an empty module path, got nil")
		}
	})

	t.Run("rejects a record with an empty dir", func(t *testing.T) {
		// `go list -m all` reports an empty directory for a module the module
		// cache has not fetched. Accepting that record would let the graph
		// audit report "no license found" for a module it never opened, which
		// is the exact failure GraphModules' `go mod download all` exists to
		// prevent. [LAW:no-silent-failure]
		if _, err := parseModuleList("github.com/example/mod\tv1.0.0\t\t\n"); err == nil {
			t.Fatal("want error for an empty module dir, got nil")
		}
	})

	t.Run("accepts an empty-version module (local replace shape)", func(t *testing.T) {
		got, err := parseModuleList("github.com/example/mod\t\t/mod/dir\t\n")
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 1 || got[0].Version != "" {
			t.Fatalf("got %+v, want one module with empty version", got)
		}
	})

	t.Run("carries the replacement when a module is replaced", func(t *testing.T) {
		// The shape this repo's own go.mod produces: the ORIGINAL coordinate
		// paired with a directory belonging to a DIFFERENT module at a
		// DIFFERENT version. The record must carry that, or a license read out
		// of that directory gets reported against a coordinate whose source
		// was never opened.
		in := "github.com/dolthub/dolt/go\tv0.40.5-0.20260314011441-62975ef6bf36\t" +
			"/mod/github.com/promptctl/dolt/go@v0.40.5-0.20260816040811-3eabc076e073\t" +
			"github.com/promptctl/dolt/go@v0.40.5-0.20260816040811-3eabc076e073\n"
		got, err := parseModuleList(in)
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d modules, want 1: %+v", len(got), got)
		}
		if !got[0].IsReplaced() {
			t.Errorf("module reports IsReplaced()=false despite a replacement: %+v", got[0])
		}
		if want := "github.com/promptctl/dolt/go@v0.40.5-0.20260816040811-3eabc076e073"; got[0].ReplacedBy != want {
			t.Errorf("ReplacedBy = %q, want %q", got[0].ReplacedBy, want)
		}
	})

	t.Run("an unreplaced module is not reported as replaced", func(t *testing.T) {
		got, err := parseModuleList("github.com/example/mod\tv1.0.0\t/mod/dir\t\n")
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if got[0].IsReplaced() {
			t.Errorf("unreplaced module reports IsReplaced()=true: %+v", got[0])
		}
	})
}
