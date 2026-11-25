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
	content := string(ScaffoldFresh("when", `when: ["enter closed"]`))
	if !strings.Contains(content, "\n"+`when: ["enter closed"]`+"\n") {
		t.Fatalf("ScaffoldFresh() = %q, want the live when line uncommented", content)
	}
	if strings.Contains(content, "# when: [") {
		t.Fatalf("ScaffoldFresh() = %q, want no redundant commented when example", content)
	}
	if !strings.Contains(content, "# labels: [needs-design, blocked]") {
		t.Fatalf("ScaffoldFresh() = %q, want the labels key shown commented as reference", content)
	}

	content = string(ScaffoldFresh("labels", `labels: ["needs-design"]`))
	if !strings.Contains(content, "# when: [enter review, exit plan, show_ticket]") {
		t.Fatalf("ScaffoldFresh() = %q, want the when key shown commented as reference", content)
	}
	if strings.Contains(content, "# labels: [") {
		t.Fatalf("ScaffoldFresh() = %q, want no redundant commented labels example", content)
	}
}

// A fresh scaffold parses, once it is filled in, to the definition its live
// line names and loads with no warning: its commented reference is the
// format this lit reads.
func TestScaffoldFreshParsesCleanly(t *testing.T) {
	def, warnings, ok := parseDefinition(string(ScaffoldFresh("when", `when: ["enter closed"]`)), "enter-closed.md", templates.SourceProject)
	if !ok || len(warnings) != 0 {
		t.Fatalf("parseDefinition(scaffold) ok=%v warnings=%v, want a clean load", ok, warnings)
	}
	if want := []Moment{Crossing{Side: Enter, State: "closed"}}; !reflect.DeepEqual(def.When, want) {
		t.Fatalf("When = %v, want %v", def.When, want)
	}
}
