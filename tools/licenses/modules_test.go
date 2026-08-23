package main

import (
	"strings"
	"testing"
)

// TestParseModuleListAcceptReject is the accept/reject table for
// parseModuleList. The producers are linkedModuleTemplate's `go list -deps`
// output and graphModuleTemplate's `go list -m all` output: tab-separated
// path/version/dir/replacement-path/replacement-version records, one per
// module, with blank lines for the records both templates skip (stdlib
// packages, the main module). Any other shape is a broken producer assumption
// and must fail loudly rather than silently dropping or misreading a module.
// [LAW:types-are-the-program]
func TestParseModuleListAcceptReject(t *testing.T) {
	t.Run("dedupes a module linked via multiple subpackages", func(t *testing.T) {
		// golang.org/x/sys links in via many import paths, but is one module.
		in := "golang.org/x/sys\tv0.43.0\t/mod/golang.org/x/sys@v0.43.0\t\t\n" +
			"golang.org/x/sys\tv0.43.0\t/mod/golang.org/x/sys@v0.43.0\t\t\n"
		got, err := parseModuleList(in)
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d modules, want 1 (deduped): %+v", len(got), got)
		}
	})

	t.Run("sorts by module path regardless of input order", func(t *testing.T) {
		in := "zzz.example.com/z\tv1\t/mod/z\t\t\n" +
			"aaa.example.com/a\tv1\t/mod/a\t\t\n"
		got, err := parseModuleList(in)
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 2 || got[0].Path != "aaa.example.com/a" || got[1].Path != "zzz.example.com/z" {
			t.Fatalf("not sorted: %+v", got)
		}
	})

	t.Run("skips blank lines (stdlib packages)", func(t *testing.T) {
		in := "\ngithub.com/example/mod\tv1.0.0\t/mod/dir\t\t\n\n"
		got, err := parseModuleList(in)
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d modules, want 1: %+v", len(got), got)
		}
	})

	t.Run("rejects a line with the wrong field count", func(t *testing.T) {
		// The arity is a contract between moduleFields (the one producer layout,
		// modules.go) and this parser. It was four fields until the
		// replacement's path and version were split apart, so a template edited
		// back toward four — or forward to six — must fail here rather than
		// shift every column by one: a module Dir read out of the replacement
		// column names a directory that does not exist, and the failure would
		// surface as "no license file found" somewhere else entirely.
		// [LAW:no-silent-failure]
		for _, line := range []string{
			"github.com/example/mod\tv1.0.0\t/mod/dir\n",
			"github.com/example/mod\tv1.0.0\t/mod/dir\t\n",
			"github.com/example/mod\tv1.0.0\t/mod/dir\t\t\t\n",
		} {
			if _, err := parseModuleList(line); err == nil {
				t.Errorf("parseModuleList accepted %q; want a malformed-line error", line)
			}
		}
	})

	t.Run("rejects a record with an empty path", func(t *testing.T) {
		if _, err := parseModuleList("\tv1.0.0\t/mod/dir\t\t\n"); err == nil {
			t.Fatal("want error for an empty module path, got nil")
		}
	})

	t.Run("rejects a record with an empty dir", func(t *testing.T) {
		// `go list -m all` reports an empty directory for a module the module
		// cache has not fetched. Accepting that record would let the graph
		// audit report "no license found" for a module it never opened, which
		// is the exact failure GraphModules' `go mod download all` exists to
		// prevent. [LAW:no-silent-failure]
		if _, err := parseModuleList("github.com/example/mod\tv1.0.0\t\t\t\n"); err == nil {
			t.Fatal("want error for an empty module dir, got nil")
		}
	})

	t.Run("accepts an empty-version module (local replace shape)", func(t *testing.T) {
		got, err := parseModuleList("github.com/example/mod\t\t/mod/dir\t\t\n")
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
		// was never opened. The pins below are a frozen illustration of that
		// shape, deliberately not kept in step with go.mod — the fork-pin
		// synchronization lives in forks_test.go, and re-quoting live pins
		// here would just add another copy for it to chase.
		in := "github.com/dolthub/dolt/go\tv0.40.5-0.20260314011441-62975ef6bf36\t" +
			"/mod/github.com/promptctl/dolt/go@v0.40.5-0.20260816040811-3eabc076e073\t" +
			"github.com/promptctl/dolt/go\tv0.40.5-0.20260816040811-3eabc076e073\n"
		got, err := parseModuleList(in)
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d modules, want 1: %+v", len(got), got)
		}
		if !got[0].IsReplaced() {
			t.Errorf("module reports IsReplaced()=false despite a replacement: %+v (kind %d)", got[0], got[0].Replacement.Kind)
		}
		// The parts are asserted individually, not just the rendered string: the
		// SBOM's descendant component needs path and version as separate fields,
		// so a parse that produced the right String() from the wrong split would
		// still emit a wrong purl.
		want := Replacement{
			Kind:    ReplacedByFork,
			Path:    "github.com/promptctl/dolt/go",
			Version: "v0.40.5-0.20260816040811-3eabc076e073",
		}
		if got[0].Replacement != want {
			t.Errorf("Replacement = %#v, want %#v", got[0].Replacement, want)
		}
		if s, wantStr := got[0].Replacement.String(), "github.com/promptctl/dolt/go@v0.40.5-0.20260816040811-3eabc076e073"; s != wantStr {
			t.Errorf("Replacement.String() = %q, want %q", s, wantStr)
		}
	})

	t.Run("carries a directory replacement, which has no version", func(t *testing.T) {
		// The other replacement shape this repo's go.mod produces: a local
		// directory (the vendored driver), where linkedModuleTemplate emits
		// Replace.Path bare because Replace.Version is empty. Pins frozen as
		// illustration, per the note on the versioned case above.
		in := "github.com/dolthub/driver\tv0.2.1-0.20260314000741-0fe74e7ee31a\t" +
			"/repo/internal/vendor/dolthub-driver\t./internal/vendor/dolthub-driver\t\n"
		got, err := parseModuleList(in)
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d modules, want 1: %+v", len(got), got)
		}
		if !got[0].IsReplaced() {
			t.Errorf("module reports IsReplaced()=false despite a directory replacement: %+v (kind %d)", got[0], got[0].Replacement.Kind)
		}
		// Kind is what the SBOM switches on to decide whether a descendant
		// component can exist at all, so the DIRECTORY shape must be
		// distinguishable from the module shape here, not merely non-empty.
		want := Replacement{Kind: ReplacedByDirectory, Path: "./internal/vendor/dolthub-driver"}
		if got[0].Replacement != want {
			t.Errorf("Replacement = %#v, want %#v", got[0].Replacement, want)
		}
		if s, wantStr := got[0].Replacement.String(), "./internal/vendor/dolthub-driver"; s != wantStr {
			t.Errorf("Replacement.String() = %q, want %q — a directory has no version to append", s, wantStr)
		}
	})

	t.Run("an unreplaced module is not reported as replaced", func(t *testing.T) {
		got, err := parseModuleList("github.com/example/mod\tv1.0.0\t/mod/dir\t\t\n")
		if err != nil {
			t.Fatalf("parseModuleList: %v", err)
		}
		if got[0].IsReplaced() {
			t.Errorf("unreplaced module reports IsReplaced()=true: %+v (kind %d)", got[0], got[0].Replacement.Kind)
		}
		if want := (Replacement{}); got[0].Replacement != want {
			t.Errorf("Replacement = %#v, want the zero value", got[0].Replacement)
		}
	})
}

// TestParseReplacementRefusesShapesGoDoesNotProduce pins the cross-check
// between path shape and version presence. `go.mod` grammar permits a
// version-less replacement target only when it is a filesystem path, so each
// combination below is one the go tool cannot emit — and each, accepted
// silently, would put a false statement into a shipped compliance artifact
// rather than merely produce odd output. A module path with no version would be
// classified as a directory replacement, and its SBOM pedigree would then
// assert "no published coordinate identifies the patched source" about a
// coordinate that is published and resolvable.
//
// Each case asserts the error MESSAGE, not merely that an error occurred:
// parseReplacement refuses from five arms, several of which would accept
// another's input if its condition were widened, so a check for non-nil alone
// would stay green through exactly that mistake.
func TestParseReplacementRefusesShapesGoDoesNotProduce(t *testing.T) {
	const modulePath = "github.com/dolthub/dolt/go"
	for _, tc := range []struct {
		name        string
		path        string
		version     string
		wantMessage string
	}{
		{
			name:        "version with no path",
			path:        "",
			version:     "v1.2.3",
			wantMessage: "with no replacement path",
		},
		{
			name:        "module path with no version",
			path:        "github.com/promptctl/dolt/go",
			version:     "",
			wantMessage: "is a module path but carries no version",
		},
		{
			name:        "directory path carrying a version",
			path:        "./internal/vendor/dolthub-driver",
			version:     "v1.2.3",
			wantMessage: "is a filesystem path but carries version",
		},
		{
			// Neither a module coordinate nor a directory target. Treating "not
			// a module path" as proof of a directory would file this under
			// ReplacedByDirectory, and its pedigree would then tell a reader
			// that no published coordinate identifies the source — about a
			// string that identifies nothing at all.
			name:        "path that is neither a module nor a directory",
			path:        "internal/vendor/dolthub-driver",
			version:     "",
			wantMessage: "neither a module path nor a filesystem path",
		},
		{
			name:        "a bare name with a version",
			path:        "notamodule",
			version:     "v1.2.3",
			wantMessage: "neither a module path nor a filesystem path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseReplacement(modulePath, tc.path, tc.version)
			if err == nil {
				t.Fatalf("parseReplacement(%q, %q, %q) = %#v, want an error", modulePath, tc.path, tc.version, got)
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Errorf("parseReplacement(%q, %q, %q) error = %q, want it to mention %q",
					modulePath, tc.path, tc.version, err, tc.wantMessage)
			}
		})
	}
}

// TestReplacementStringRefusesAnUnknownKind pins the loud arm every renderer
// shares. Go has no switch exhaustiveness check and this repo runs no linter
// that adds one, so a fourth ReplacementKind would otherwise fall through a
// `default` and render as NOT REPLACED — which is not a cosmetic bug but the
// silent non-disclosure this whole package exists to prevent, reintroduced for
// the new shape and shipped in a compliance artifact.
//
// Before this arm was made loud, `default: return r.Path` was the one mutation
// in the package that survived the entire suite.
func TestReplacementStringRefusesAnUnknownKind(t *testing.T) {
	unknown := Replacement{Kind: ReplacementKind(99), Path: "github.com/example/whatever"}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("String() returned normally for an unhandled ReplacementKind; a new shape would render as not-replaced")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is %T, want a string", r)
		}
		// The message must name the kind and point at the fix, because the only
		// reader who ever sees it is someone mid-way through adding a shape.
		for _, want := range []string{"ReplacementKind 99", "teach every renderer"} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic message %q does not mention %q", msg, want)
			}
		}
	}()

	_ = unknown.String()
}

// TestParseReplacementSeparatesAForkFromAVersionPin pins the distinction that
// keeps the SBOM from asserting a component descends from itself.
//
// `replace x => x v1.2.3` is the ordinary way to force a version, and it
// reaches this parser looking exactly like a fork except that the replacement
// path equals the module being replaced. Before the kinds were split, that
// produced a pedigree.descendants entry naming the same module — a claim that
// x is a fork of x, in a structured field, about a go.mod idiom anyone might
// add tomorrow. lit's go.mod has no such replace today, which is precisely why
// the rule needs a test rather than a reader.
func TestParseReplacementSeparatesAForkFromAVersionPin(t *testing.T) {
	const modulePath = "github.com/spf13/viper"

	pin, err := parseReplacement(modulePath, modulePath, "v1.19.0")
	if err != nil {
		t.Fatalf("parseReplacement for a version pin: %v", err)
	}
	if pin.Kind != ReplacedByVersion {
		t.Errorf("`replace x => x v1.19.0` parsed as kind %d, want ReplacedByVersion (%d)", pin.Kind, ReplacedByVersion)
	}

	fork, err := parseReplacement(modulePath, "github.com/promptctl/viper", "v1.19.0")
	if err != nil {
		t.Fatalf("parseReplacement for a fork: %v", err)
	}
	if fork.Kind != ReplacedByFork {
		t.Errorf("a different path parsed as kind %d, want ReplacedByFork (%d)", fork.Kind, ReplacedByFork)
	}

	// Both still render as a coordinate — the difference is what the artifacts
	// are allowed to CLAIM about it, not how it is spelled.
	if pin.String() != modulePath+"@v1.19.0" {
		t.Errorf("version pin renders as %q, want the substitute coordinate", pin.String())
	}
}
