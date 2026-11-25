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
when:
  - enter open
  - enter in_progress
  - exit in_progress
  - show_ticket
labels:
  - needs-design
  - blocked
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
		ID:   "needs-design-review",
		Name: "Design review reminder",
		When: []Moment{
			Crossing{Side: Enter, State: "open"},
			Crossing{Side: Enter, State: "in_progress"},
			Crossing{Side: Exit, State: "in_progress"},
			EventShowTicket,
		},
		Labels: []string{"needs-design", "blocked"},
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
	def, _, ok := parseDefinition("---\nwhen: [show_backlog]\n---\nbody", "review tasks/design check.md", templates.SourceGlobal)
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
		"unterminated frontmatter": "---\nwhen: [show_backlog]\nno closing delimiter",
		"non-mapping frontmatter":  "---\n- a\n- b\n---\nbody",
		"scalar frontmatter":       "---\nhello\n---\nbody",
		"when not a sequence":      "---\nwhen: enter closed\n---\nbody",
		"when entry a mapping":     "---\nwhen:\n  - {name: closed}\n---\nbody",
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

// An event outside the catalog — one a newer lit may fire, or one an older
// lit fired for a change of state — loads as authored, with a warning that
// says how a change of state is written; no command here fires it. The rest
// of the list still binds, since entries are alternatives.
func TestParseDefinitionUnknownEventWarnsButLoads(t *testing.T) {
	for _, name := range []string{"warp_drive_engaged", "work_started", "phase_set", "work_finished", "ticket_closed", "ticket_reopened"} {
		t.Run(name, func(t *testing.T) {
			def, warnings, ok := parseDefinition("---\nwhen: ["+name+", enter closed]\n---\nbody", "old.md", templates.SourceProject)
			if !ok {
				t.Fatal("parseDefinition() not ok; unknown events must not reject the file")
			}
			want := []Moment{Event(name), Crossing{Side: Enter, State: "closed"}}
			if !reflect.DeepEqual(def.When, want) {
				t.Fatalf("When = %v, want %v, the authored entry preserved", def.When, want)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0].Message, name) || !strings.Contains(warnings[0].Message, "enter <state> or exit <state>") {
				t.Fatalf("warnings = %v, want one unknown-event warning naming %s and the crossing spelling", warnings, name)
			}
		})
	}
}

// A file carrying a key this lit does not read never fires — not even on the
// keys it does read, which the unread key may have narrowed — and it says so,
// naming the key and the spelling lit reads. It still holds its id.
func TestParseDefinitionUnreadKeysNeverFire(t *testing.T) {
	cases := map[string]string{
		"events":                 "---\nid: done\nevents: [work_finished]\n---\nbody",
		"states":                 "---\nstates:\n  - {name: plan, when: exit}\n---\nbody",
		"beside a key it reads":  "---\nevents: [work_finished]\nlabels: [needs-design]\nwhen: [show_ticket]\n---\nbody",
		"a cased key":            "---\nWhen: [enter closed]\n---\nbody",
		"a key from a newer lit": "---\nwhen: [enter closed]\nwhere: phase=plan\n---\nbody",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			def, warnings, ok := parseDefinition(content, "done.md", templates.SourceProject)
			if !ok {
				t.Fatalf("parseDefinition() not ok, warnings = %v; the file must still hold its id", warnings)
			}
			if def.ID != "done" {
				t.Fatalf("ID = %q, want the file's id held", def.ID)
			}
			everything := Occasion{
				Moments: []Moment{EventShowTicket, Crossing{Side: Enter, State: "closed"}, Crossing{Side: Exit, State: "plan"}},
				Labels:  []string{"needs-design"},
			}
			if def.Matches(everything) {
				t.Fatalf("definition %+v fired; a file lit cannot fully read never fires", def)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "does not read") || !strings.Contains(warnings[0].Message, "when: [enter closed]") {
				t.Fatalf("warnings = %v, want one naming the unread key and the when: spelling", warnings)
			}
		})
	}
}

func TestParseDefinitionNormalization(t *testing.T) {
	content := "---\nid: '  spaced  '\nlabels: ['  a  ', '', '   ']\nwhen: ['', ' Show_Ticket ', '  ENTER   In_Progress ']\n---\n\n  body text  \n"
	def, warnings, ok := parseDefinition(content, "n.md", templates.SourceProject)
	if !ok {
		t.Fatalf("parseDefinition() not ok, warnings = %v", warnings)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none — a case variant is not an unknown event", warnings)
	}
	if def.ID != "spaced" {
		t.Fatalf("ID = %q, want trimmed explicit id", def.ID)
	}
	if !reflect.DeepEqual(def.Labels, []string{"a"}) {
		t.Fatalf("Labels = %v, want empties dropped", def.Labels)
	}
	if want := []Moment{EventShowTicket, Crossing{Side: Enter, State: "in_progress"}}; !reflect.DeepEqual(def.When, want) {
		t.Fatalf("When = %v, want %v: lowercased, whitespace collapsed, empties dropped", def.When, want)
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

// ParseMoment reads every spelling a when: entry takes; a state is everything
// after the side word, so a custom stage may hold a colon, a comma or a space.
func TestParseMoment(t *testing.T) {
	cases := map[string]Moment{
		"enter closed":          Crossing{Side: Enter, State: "closed"},
		"exit plan":             Crossing{Side: Exit, State: "plan"},
		"Exit  Review":          Crossing{Side: Exit, State: "review"},
		"enter deploy:staging":  Crossing{Side: Enter, State: "deploy:staging"},
		"enter foo,bar":         Crossing{Side: Enter, State: "foo,bar"},
		"enter ready  to  pull": Crossing{Side: Enter, State: "ready to pull"},
		"show_ticket":           EventShowTicket,
		"enter":                 Event("enter"),
	}
	for text, want := range cases {
		got, ok := ParseMoment(text)
		if !ok || got != want {
			t.Errorf("ParseMoment(%q) = %v, %v; want %v", text, got, ok, want)
		}
		if again, _ := ParseMoment(got.String()); again != got {
			t.Errorf("ParseMoment(%q.String()) = %v, want it to read back as itself", got, again)
		}
	}
	if got, ok := ParseMoment("  "); ok {
		t.Errorf("ParseMoment(blank) = %v, want no moment", got)
	}
}

func TestParseDefinitionCRLF(t *testing.T) {
	def, _, ok := parseDefinition("---\r\nwhen: [show_backlog]\r\n---\r\nbody\r\n", "crlf.md", templates.SourceProject)
	if !ok {
		t.Fatal("parseDefinition() not ok on CRLF input")
	}
	if !reflect.DeepEqual(def.When, []Moment{EventShowBacklog}) {
		t.Fatalf("When = %v, want [show_backlog]", def.When)
	}
}

// Delimiter lines tolerate trailing horizontal whitespace, per the frontmatter
// convention — a trailing space must not silently degrade the file to inert.
func TestParseDefinitionDelimiterTrailingWhitespace(t *testing.T) {
	def, warnings, ok := parseDefinition("--- \nwhen: [show_backlog]\n---\t\nbody", "ws.md", templates.SourceProject)
	if !ok {
		t.Fatalf("parseDefinition() not ok, warnings = %v", warnings)
	}
	if !reflect.DeepEqual(def.When, []Moment{EventShowBacklog}) {
		t.Fatalf("When = %v, want frontmatter recognized despite trailing whitespace", def.When)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}
