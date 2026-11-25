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

func TestScaffoldFreshOmitsTheLiveKeysCommentedExample(t *testing.T) {
	raw, live, err := ScaffoldFresh("enter closed")
	content := string(raw)
	if err != nil || live != `when: ["enter closed"]` {
		t.Fatalf("ScaffoldFresh(enter closed) live = %q, err = %v; want the when line", live, err)
	}
	if !strings.Contains(content, "\n"+live+"\n") {
		t.Fatalf("ScaffoldFresh() = %q, want the live when line uncommented", content)
	}
	if strings.Contains(content, "# when: [") {
		t.Fatalf("ScaffoldFresh() = %q, want no redundant commented when example", content)
	}
	if !strings.Contains(content, "# labels: [needs-design, blocked]") {
		t.Fatalf("ScaffoldFresh() = %q, want the labels key shown commented as reference", content)
	}

	raw, live, err = ScaffoldFresh("Needs-Design")
	content = string(raw)
	if err != nil || live != `labels: ["needs-design"]` {
		t.Fatalf("ScaffoldFresh(Needs-Design) live = %q, err = %v; want the canonical labels line", live, err)
	}
	if !strings.Contains(content, "# when: [enter review, exit plan, show_ticket]") {
		t.Fatalf("ScaffoldFresh() = %q, want the when key shown commented as reference", content)
	}
	if strings.Contains(content, "# labels: [") {
		t.Fatalf("ScaffoldFresh() = %q, want no redundant commented labels example", content)
	}
}

// A fresh scaffold parses, once it is filled in, to the definition its live
// line names and loads with no warning: its commented reference is the
// format this lit reads, and the point is written in the spelling it reads
// back as.
func TestScaffoldFreshParsesCleanly(t *testing.T) {
	cases := map[string]Keys{
		"enter closed":      {When: []Moment{Crossing{Side: Enter, State: "closed"}}},
		"Exit  In-Progress": {When: []Moment{Crossing{Side: Exit, State: "in_progress"}}},
		"show_ticket":       {When: []Moment{EventShowTicket}},
		"Needs-Design":      {Labels: []string{"needs-design"}},
	}
	for point, want := range cases {
		raw, _, err := ScaffoldFresh(point)
		if err != nil {
			t.Fatalf("ScaffoldFresh(%q) error = %v", point, err)
		}
		def, warnings := parseDefinition(string(raw), "fresh.md", templates.SourceProject)
		if len(warnings) != 0 || !reflect.DeepEqual(def.Activation, want) {
			t.Fatalf("parseDefinition(ScaffoldFresh(%q)) = %+v, warnings %v; want %+v and a clean load", point, def.Activation, warnings, want)
		}
	}
}

// A point that is neither a when: entry nor a label is refused rather than
// scaffolded into a file that could never fire.
func TestScaffoldFreshRefusesAPointThatIsNeither(t *testing.T) {
	for _, point := range []string{"  ", "a,b"} {
		if raw, live, err := ScaffoldFresh(point); err == nil {
			t.Fatalf("ScaffoldFresh(%q) = %q, %q; want a refusal", point, raw, live)
		}
	}
}
