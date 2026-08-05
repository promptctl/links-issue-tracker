package workflows

import (
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/templates"
)

func TestParseDefinitionFullFrontmatter(t *testing.T) {
	content := `---
id: needs-design-review
name: Design review reminder
labels:
  - needs-design
  - blocked
states:
  - open
  - name: in_progress
    when: enter
  - name: in_progress
    when: exit
events:
  - show_ticket
---

Remember to review the design doc first.
`
	def, warnings, ok := parseDefinition(content, "review/design.md", templates.SourceProject)
	if !ok {
		t.Fatalf("parseDefinition() not ok, warnings = %v", warnings)
	}
	if len(warnings) != 0 {
		t.Fatalf("parseDefinition() warnings = %v, want none", warnings)
	}
	want := Definition{
		ID:     "needs-design-review",
		Name:   "Design review reminder",
		Labels: []string{"needs-design", "blocked"},
		States: []StateActivation{
			{State: "open", When: WhenEnter},
			{State: "in_progress", When: WhenEnter},
			{State: "in_progress", When: WhenExit},
		},
		Events: []Event{EventShowTicket},
		Body:   "Remember to review the design doc first.",
		Source: templates.SourceProject,
		Path:   "review/design.md",
	}
	if !reflect.DeepEqual(def, want) {
		t.Fatalf("parseDefinition() = %+v, want %+v", def, want)
	}
}

func TestParseDefinitionDefaultID(t *testing.T) {
	// The default ID is the layer-relative path, ".md" dropped, spaces → "_".
	def, _, ok := parseDefinition("---\nevents: [show_backlog]\n---\nbody", "review tasks/design check.md", templates.SourceGlobal)
	if !ok {
		t.Fatal("parseDefinition() not ok")
	}
	if def.ID != "review_tasks/design_check" {
		t.Fatalf("default ID = %q, want %q", def.ID, "review_tasks/design_check")
	}
}

func TestParseDefinitionNoFrontmatterIsInertWithWarning(t *testing.T) {
	def, warnings, ok := parseDefinition("just some notes\n", "notes.md", templates.SourceProject)
	if !ok {
		t.Fatal("parseDefinition() not ok; a plain markdown file must still load")
	}
	if !def.Inert() {
		t.Fatalf("definition = %+v, want inert", def)
	}
	if def.Body != "just some notes" {
		t.Fatalf("Body = %q, want the whole file", def.Body)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "inert") {
		t.Fatalf("warnings = %v, want one inert warning", warnings)
	}
}

func TestParseDefinitionMalformed(t *testing.T) {
	cases := map[string]string{
		"unterminated frontmatter": "---\nevents: [show_backlog]\nno closing delimiter",
		"non-mapping frontmatter":  "---\n- a\n- b\n---\nbody",
		"scalar frontmatter":       "---\nhello\n---\nbody",
		"invalid when":             "---\nstates:\n  - name: open\n    when: banana\n---\nbody",
		"invalid state entry kind": "---\nstates:\n  - [open]\n---\nbody",
		"nameless state mapping":   "---\nstates:\n  - when: enter\n---\nbody",
		"labels not a sequence":    "---\nlabels: 17\n---\nbody",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, warnings, ok := parseDefinition(content, "bad.md", templates.SourceProject)
			if ok {
				t.Fatal("parseDefinition() ok, want malformed → skipped")
			}
			if len(warnings) != 1 {
				t.Fatalf("warnings = %v, want exactly one", warnings)
			}
			if warnings[0].Path != "bad.md" || warnings[0].Source != templates.SourceProject {
				t.Fatalf("warning = %+v, want path/source attached", warnings[0])
			}
		})
	}
}

func TestParseDefinitionUnknownEventWarnsButLoads(t *testing.T) {
	def, warnings, ok := parseDefinition("---\nevents: [warp_drive_engaged]\n---\nbody", "future.md", templates.SourceProject)
	if !ok {
		t.Fatal("parseDefinition() not ok; unknown events must not reject the file")
	}
	if len(def.Events) != 1 || def.Events[0] != Event("warp_drive_engaged") {
		t.Fatalf("Events = %v, want the authored event preserved", def.Events)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "warp_drive_engaged") {
		t.Fatalf("warnings = %v, want one unknown-event warning", warnings)
	}
}

func TestParseDefinitionNormalization(t *testing.T) {
	content := "---\nid: '  spaced  '\nlabels: ['  a  ', '', '   ']\nstates: ['', '  open ']\nevents: [' show_ticket ', '']\n---\n\n  body text  \n"
	def, warnings, ok := parseDefinition(content, "n.md", templates.SourceProject)
	if !ok {
		t.Fatalf("parseDefinition() not ok, warnings = %v", warnings)
	}
	if def.ID != "spaced" {
		t.Fatalf("ID = %q, want trimmed explicit id", def.ID)
	}
	if !reflect.DeepEqual(def.Labels, []string{"a"}) {
		t.Fatalf("Labels = %v, want empties dropped", def.Labels)
	}
	if !reflect.DeepEqual(def.States, []StateActivation{{State: "open", When: WhenEnter}}) {
		t.Fatalf("States = %v, want empties dropped", def.States)
	}
	if !reflect.DeepEqual(def.Events, []Event{EventShowTicket}) {
		t.Fatalf("Events = %v, want trimmed, empties dropped", def.Events)
	}
	if def.Body != "body text" {
		t.Fatalf("Body = %q, want trimmed", def.Body)
	}
}

// Frontmatter labels are stamped into the store's canonical form at parse
// time, so an authored "Needs-Design" matches the persisted "needs-design".
func TestParseDefinitionLabelsCanonicalized(t *testing.T) {
	def, warnings, ok := parseDefinition("---\nlabels: ['Needs-Design', 'has,comma']\n---\nbody", "l.md", templates.SourceProject)
	if !ok {
		t.Fatalf("parseDefinition() not ok, warnings = %v", warnings)
	}
	if !reflect.DeepEqual(def.Labels, []string{"needs-design"}) {
		t.Fatalf("Labels = %v, want lowercased canonical form", def.Labels)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "has,comma") {
		t.Fatalf("warnings = %v, want one can-never-match warning for the comma label", warnings)
	}
}

// State names resolve case variants to the lifecycle's lowercase spelling,
// in both authored shapes.
func TestParseDefinitionStatesCanonicalized(t *testing.T) {
	content := "---\nstates:\n  - ' In_Progress '\n  - name: ' OPEN '\n    when: exit\n---\nbody"
	def, warnings, ok := parseDefinition(content, "s.md", templates.SourceProject)
	if !ok {
		t.Fatalf("parseDefinition() not ok, warnings = %v", warnings)
	}
	want := []StateActivation{
		{State: "in_progress", When: WhenEnter},
		{State: "open", When: WhenExit},
	}
	if !reflect.DeepEqual(def.States, want) {
		t.Fatalf("States = %v, want %v", def.States, want)
	}
}

// Event entries resolve case variants to the catalog's lowercase canonical
// form, symmetric with label canonicalization.
func TestParseDefinitionEventsCanonicalized(t *testing.T) {
	def, warnings, ok := parseDefinition("---\nevents: [' Show_Ticket ']\n---\nbody", "e.md", templates.SourceProject)
	if !ok {
		t.Fatalf("parseDefinition() not ok, warnings = %v", warnings)
	}
	if !reflect.DeepEqual(def.Events, []Event{EventShowTicket}) {
		t.Fatalf("Events = %v, want lowercased canonical form", def.Events)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none — a case variant is not an unknown event", warnings)
	}
}

func TestParseDefinitionCRLF(t *testing.T) {
	def, _, ok := parseDefinition("---\r\nevents: [show_backlog]\r\n---\r\nbody\r\n", "crlf.md", templates.SourceProject)
	if !ok {
		t.Fatal("parseDefinition() not ok on CRLF input")
	}
	if len(def.Events) != 1 || def.Events[0] != EventShowBacklog {
		t.Fatalf("Events = %v, want [show_backlog]", def.Events)
	}
}

func TestParseDefinitionUnknownKeysTolerated(t *testing.T) {
	// Forward compatibility: a file authored for a newer lit still loads with
	// the keys this binary understands.
	def, warnings, ok := parseDefinition("---\nevents: [show_ticket]\nfuture_key: whatever\n---\nbody", "fwd.md", templates.SourceProject)
	if !ok || len(warnings) != 0 {
		t.Fatalf("parseDefinition() ok=%v warnings=%v, want clean load", ok, warnings)
	}
	if len(def.Events) != 1 {
		t.Fatalf("Events = %v, want the known key honored", def.Events)
	}
}
