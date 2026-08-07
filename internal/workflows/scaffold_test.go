package workflows

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/templates"
)

func TestRawDefaultReadsEmbeddedDoneVerbatim(t *testing.T) {
	raw, err := RawDefault(templates.SourceEmbedded, "done.md")
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
	content := "---\nid: my-global\nevents: [show_ticket]\n---\nglobal body"
	if err := os.WriteFile(filepath.Join(globalWorkflows, "custom.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	raw, err := RawDefault(templates.SourceGlobal, "custom.md")
	if err != nil {
		t.Fatalf("RawDefault(global, custom.md) error = %v", err)
	}
	if string(raw) != content {
		t.Fatalf("RawDefault(global, custom.md) = %q, want %q", raw, content)
	}
}

func TestRawDefaultRejectsProjectLayer(t *testing.T) {
	if _, err := RawDefault(templates.SourceProject, "whatever.md"); err == nil {
		t.Fatal("RawDefault(project, ...) error = nil, want an error (project is the override target, not a source)")
	}
}

func TestScaffoldFreshOmitsTheLiveDimensionsCommentedExample(t *testing.T) {
	content := string(ScaffoldFresh("events", "events: [work_started]"))
	if !strings.Contains(content, "events: [work_started]") {
		t.Fatalf("ScaffoldFresh() = %q, want the live events line uncommented", content)
	}
	if strings.Contains(content, "# events: [work_started]") {
		t.Fatalf("ScaffoldFresh() = %q, want no redundant commented events example", content)
	}
	if !strings.Contains(content, "# labels: [needs-design, blocked]") {
		t.Fatalf("ScaffoldFresh() = %q, want the labels dimension shown commented as reference", content)
	}
	if !strings.Contains(content, "# states: [open]") {
		t.Fatalf("ScaffoldFresh() = %q, want the states dimension shown commented as reference", content)
	}
}
