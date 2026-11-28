package workflows

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/templates"
)

func TestRawDefaultReadsEmbeddedDoneVerbatim(t *testing.T) {
	raw, err := RawDefault(testEnv(), templates.SourceEmbedded, "done.md")
	if err != nil {
		t.Fatalf("RawDefault(embedded, done.md) error = %v", err)
	}
	if !strings.Contains(string(raw), "id: done") {
		t.Fatalf("RawDefault(embedded, done.md) = %q, want the embedded done.md content", raw)
	}
}

func TestRawDefaultReadsGlobalLayer(t *testing.T) {
	_, globalWorkflows := isolate(t)
	if err := os.MkdirAll(globalWorkflows, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", globalWorkflows, err)
	}
	content := "---\nid: my-global\nwhen: [show_ticket]\n---\nglobal body"
	if err := os.WriteFile(filepath.Join(globalWorkflows, "custom.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	raw, err := RawDefault(testEnv(), templates.SourceGlobal, "custom.md")
	if err != nil {
		t.Fatalf("RawDefault(global, custom.md) error = %v", err)
	}
	if string(raw) != content {
		t.Fatalf("RawDefault(global, custom.md) = %q, want %q", raw, content)
	}
}

func TestRawDefaultRejectsProjectLayer(t *testing.T) {
	if _, err := RawDefault(testEnv(), templates.SourceProject, "whatever.md"); err == nil {
		t.Fatal("RawDefault(project, ...) error = nil, want an error (project is the override target, not a source)")
	}
}

// A key the scaffold binds is written live and a key it leaves out is written
// commented, as a reference to the format.
func TestScaffoldFreshWritesEachKeyLiveOrCommented(t *testing.T) {
	cases := map[string]struct {
		when          []Moment
		labels        []string
		live, comment []string
	}{
		"a moment":  {[]Moment{entering("closed")}, nil, []string{`when: ["enter closed"]`}, []string{"# labels: [needs-design, blocked]"}},
		"a label":   {nil, []string{"needs-design"}, []string{`labels: ["needs-design"]`}, []string{"# when: [enter review, exit plan, show_ticket]  # entering or exiting a state, or an event"}},
		"both keys": {[]Moment{exiting("plan"), EventShowTicket}, []string{"a", "b"}, []string{`when: ["exit plan", "show_ticket"]`, `labels: ["a", "b"]`}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			content := string(ScaffoldFresh(tc.when, tc.labels))
			for _, line := range append(tc.live, tc.comment...) {
				if !strings.Contains(content, "\n"+line+"\n") {
					t.Fatalf("ScaffoldFresh() = %q, want the line %q", content, line)
				}
			}
			if len(tc.when) > 0 && strings.Contains(content, "\n# when: [") || len(tc.labels) > 0 && strings.Contains(content, "\n# labels: [") {
				t.Fatalf("ScaffoldFresh() = %q, want no commented example beside a live key", content)
			}
		})
	}
}

// A fresh scaffold parses to the definition its keys name and loads with no
// warning: its commented reference is the format this lit reads, and each
// entry is written in the spelling it reads back as.
func TestScaffoldFreshParsesCleanly(t *testing.T) {
	cases := map[string]struct {
		when   []Moment
		labels []string
	}{
		"a crossing": {[]Moment{entering("closed")}, nil},
		"a phase":    {[]Moment{exiting("in_progress"), entering("review")}, nil},
		"an event":   {[]Moment{EventShowTicket}, nil},
		"a label":    {nil, []string{"needs-design"}},
		"both keys":  {[]Moment{entering("archived")}, []string{"needs-design", "exit plan"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			def, warnings := parseDefinition(string(ScaffoldFresh(tc.when, tc.labels)), "fresh.md", templates.SourceProject)
			if want := firesOn(tc.when, tc.labels); len(warnings) != 0 || !reflect.DeepEqual(def.Activation, want) {
				t.Fatalf("parseDefinition(ScaffoldFresh()) = %+v, warnings %v; want %+v and a clean load", def.Activation, warnings, want)
			}
		})
	}
}
