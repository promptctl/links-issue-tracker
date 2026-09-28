package docnames

import (
	"os"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/promptctl/links-issue-tracker/internal/docclaims"
)

// repoRoot is where the gate runs from: this package sits two levels below the
// repository root, and the check is about the real corpus.
const repoRoot = "../.."

// TestEveryNamedIdentifierExists is the gate this package exists to be. A
// failure is a chapter naming Go that no Go file has, or an exclusion that no
// longer excuses anything; each line says which, and what fixes it.
func TestEveryNamedIdentifierExists(t *testing.T) {
	r, err := CheckTree(os.DirFS(repoRoot))
	if err != nil {
		t.Fatalf("CheckTree: %v", err)
	}
	for _, n := range r.Missing {
		t.Errorf("%s:%d: `%s` (in `%s`) is not an identifier in any Go file — name what the code does now, or, if it is legitimately outside this tree, add it to %s with the reason",
			n.Doc, n.Line, n.Name, n.Text, ExclusionsFile)
	}
	for _, s := range r.Stale {
		why := "no chapter writes it any more"
		if s.Declared {
			why = "the tree now has it, so it needs no excuse"
		}
		t.Errorf("%s:%d: `%s` is listed but %s — delete the line", ExclusionsFile, s.Line, s.Name, why)
	}
}

// fixture is a tree with one real function, a name that survives only in a
// comment, one only in a string, a nested module the root does not replace,
// and a testdata file — each a place a name can look present without being so.
func fixture(doc, exclusions string) fstest.MapFS {
	return fstest.MapFS{
		"go.mod":                     {Data: []byte("module example.com/m\n\ngo 1.22\n")},
		"a/a.go":                     {Data: []byte("package a\n\n// commentOnly was deleted.\nfunc realFunc() string { return \"stringOnly\" }\n")},
		"a/testdata/t.go":            {Data: []byte("package t\nfunc testdataOnly() {}\n")},
		"other/go.mod":               {Data: []byte("module example.com/other\n")},
		"other/o.go":                 {Data: []byte("package other\nfunc otherModuleOnly() {}\n")},
		docclaims.SpecDir + "/ch.md": {Data: []byte(doc)},
		ExclusionsFile:               {Data: []byte(exclusions)},
	}
}

func missingTexts(r Report) []string {
	var out []string
	for _, n := range r.Missing {
		out = append(out, n.Text)
	}
	return out
}

// TestAGhostNameIsReported is the mutation control on the gate: a check that
// can never report anything passes the real corpus as easily as a correct one.
// Every place a name can look present without being an identifier in this
// tree is paired with a real one, so a check that merely greps would fail here.
func TestAGhostNameIsReported(t *testing.T) {
	doc := "Calls `realFunc`, `a.realFunc()` and `realFunc(ghostArg)`.\n" +
		"Also `commentOnly`, `stringOnly`, `testdataOnly`, `otherModuleOnly`, `ghostFunc`, `realFunc.ghostField` and `ghostCall(x, y)`.\n" +
		"Excused: `json.Encoder` and `json.Encoder(w)`.\n"
	r, err := CheckTree(fixture(doc, "json.Encoder  standard library\n"))
	if err != nil {
		t.Fatalf("CheckTree: %v", err)
	}
	want := []string{"commentOnly", "stringOnly", "testdataOnly", "otherModuleOnly", "ghostFunc", "realFunc.ghostField", "ghostCall(x, y)"}
	if got := missingTexts(r); !slices.Equal(got, want) {
		t.Errorf("Missing = %q, want %q", got, want)
	}
	if len(r.Stale) != 0 {
		t.Errorf("Stale = %v, want none", r.Stale)
	}
}

// TestAReplacedModuleIsInTheTree pairs with the nested-module case above: a
// module go.mod replaces onto a local path is source this repository carries.
func TestAReplacedModuleIsInTheTree(t *testing.T) {
	fsys := fixture("Uses `otherModuleOnly`.\n", "")
	fsys["go.mod"] = &fstest.MapFile{Data: []byte("module example.com/m\n\ngo 1.22\n\nreplace example.com/other => ./other\n")}
	r, err := CheckTree(fsys)
	if err != nil {
		t.Fatalf("CheckTree: %v", err)
	}
	if !r.Clean() {
		t.Errorf("report = %+v, want clean", r)
	}
}

// TestStaleExclusionsAreReported holds the exclusion list to the corpus in both
// directions, so it cannot grow into a second, unchecked allowlist.
func TestStaleExclusionsAreReported(t *testing.T) {
	exclusions := "# commentary\n\njson.Encoder\tstandard library\nunwritten.Name  no chapter writes this\nrealFunc  but the tree has it\n"
	r, err := CheckTree(fixture("`json.Encoder` and `realFunc`.\n", exclusions))
	if err != nil {
		t.Fatalf("CheckTree: %v", err)
	}
	got := map[string]bool{}
	for _, s := range r.Stale {
		got[s.Name] = s.Declared
	}
	want := map[string]bool{"unwritten.Name": false, "realFunc": true}
	if len(got) != len(want) || got["unwritten.Name"] != false || got["realFunc"] != true {
		t.Errorf("Stale = %v, want %v", got, want)
	}
	if len(r.Missing) != 0 {
		t.Errorf("Missing = %q, want none", missingTexts(r))
	}
}

// TestNameShape is the accept/reject table for what the check judges. The
// rejected rows are the corpus's other backticked vocabulary, which the shape
// must leave alone or the gate drowns in it.
func TestNameShape(t *testing.T) {
	for _, tc := range []struct {
		span string
		name string // "": not judged
	}{
		{"runSyncPush", "runSyncPush"},
		{"IDMap", "IDMap"},
		{"Issue", "Issue"},
		{"app.streamTokens", "app.streamTokens"},
		{"store.Open()", "store.Open"},
		{"issueOrdering(filter.SortBy)", "issueOrdering"},
		{"a.CreatedAt.Compare(b.CreatedAt)", "a.CreatedAt.Compare"},
		{"run()", ""},
		{"dolt_log('HEAD')", ""},
		{"VARCHAR(64)", ""},
		{"open", ""},
		{"GOCACHE", ""},
		{"issue_type", ""},
		{"mkdocs.yml", ""},
		{"lit next", ""},
		{"--take", ""},
		{"cli.go", ""},
		{"[]SortSpec", ""},
		{"Issue{ID: x}", ""},
	} {
		n, ok := parseName("d.md", docclaims.Span{Text: tc.span, Line: 1})
		if ok != (tc.name != "") || n.Name != tc.name {
			t.Errorf("parseName(%q) = %q, %v; want %q", tc.span, n.Name, ok, tc.name)
		}
	}
}

// TestExclusionLinesAreRefusedWithoutAReason pins the three ways an entry is
// malformed: no reason, which leaves the next reader nothing to judge it by; a
// duplicate, which a deletion would only half-remove; and a span that is not a
// bare name, which could never match one.
func TestExclusionLinesAreRefusedWithoutAReason(t *testing.T) {
	for _, src := range []string{"json.Encoder\n", "json.Encoder  stdlib\njson.Encoder  again\n", "Bash(lit *)  a span, not a name\n"} {
		if _, err := ParseExclusions(src); err == nil {
			t.Errorf("ParseExclusions(%q) accepted it", src)
		}
	}
}

// TestSpanLinesPointAtTheSpan: the gate's report sends a reader to a line, so
// the line must be the span's own, including past a fence and a double span.
func TestSpanLinesPointAtTheSpan(t *testing.T) {
	doc := "intro\n```\n`fencedName`\n```\n``a `b` c`` then `firstName`\n\n`secondName`\n"
	names, err := Names(fstest.MapFS{"d.md": {Data: []byte(doc)}}, []string{"d.md"})
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	var got []int
	for _, n := range names {
		got = append(got, n.Line)
	}
	if want := []int{5, 7}; !slices.Equal(got, want) {
		t.Errorf("lines = %v, want %v (names %+v)", got, want, names)
	}
}
