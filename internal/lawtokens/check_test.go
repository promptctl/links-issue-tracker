package lawtokens

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/promptctl/links-issue-tracker/internal/lawtokens/tokenindex"
)

func TestCheckFilesNamesEveryInventedTokenByFileAndLine(t *testing.T) {
	fsys := fstest.MapFS{
		"clean.go":    {Data: []byte("// " + brackets("LAW", "single-enforcer") + " one checkpoint\n")},
		"invented.go": {Data: []byte("package x\n\n// " + brackets("LAW", "no-silent-fallbacks") + "\n")},
		"docs/a.md":   {Data: []byte("fine\n" + brackets("FRAMING", "representation") + " and " + brackets("LAW", "errors") + "\n")},
	}

	violations, err := CheckFiles(fsys, []string{"clean.go", "invented.go", "docs/a.md"})
	if err != nil {
		t.Fatalf("CheckFiles: %v", err)
	}

	var got []string
	for _, v := range violations {
		got = append(got, v.String())
	}
	want := []string{
		"invented.go:3: " + brackets("LAW", "no-silent-fallbacks"),
		"docs/a.md:2: " + brackets("LAW", "errors"),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("violations =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCheckFilesRefusesAFileItCannotRead(t *testing.T) {
	_, err := CheckFiles(fstest.MapFS{}, []string{"missing.go"})
	if err == nil || !strings.Contains(err.Error(), "missing.go") {
		t.Fatalf("CheckFiles error = %v, want one naming missing.go", err)
	}
}

func TestReportListsViolationsAndSaysWhatToDo(t *testing.T) {
	report := Report([]Violation{
		{Path: "a.go", Marker: Marker{Namespace: "LAW", Token: "no-silent-fallbacks", Line: 7}},
	})

	for _, want := range []string{
		"found 1 ",
		"  a.go:7: " + brackets("LAW", "no-silent-fallbacks"),
		"never invent one",
		tokenindex.UpstreamURL,
		"just lawtokens-sync",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("Report does not contain %q:\n%s", want, report)
		}
	}
}
