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
	def, warnings := parseDefinition(content, "review/design.md", templates.SourceProject)
	if len(warnings) != 0 {
		t.Fatalf("parseDefinition() warnings = %v, want none", warnings)
	}
	want := Definition{
		ID:   "needs-design-review",
		Name: "Design review reminder",
		Activation: firesOn(
			[]Moment{
				entering("open"),
				entering("in_progress"),
				exiting("in_progress"),
				EventShowTicket,
			},
			[]string{"needs-design", "blocked"},
		),
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
	def, _ := parseDefinition("---\nwhen: [show_backlog]\n---\nbody", "review tasks/design check.md", templates.SourceGlobal)
	if def.ID != "review_tasks/design_check" {
		t.Fatalf("default ID = %q, want %q", def.ID, "review_tasks/design_check")
	}
}

func TestParseDefinitionNoFrontmatterNeverFiresWithWarning(t *testing.T) {
	def, warnings := parseDefinition("just some notes\n", "notes.md", templates.SourceProject)
	if !def.Activation.Never() {
		t.Fatalf("definition = %+v, want one that never fires", def)
	}
	if def.Body != "just some notes" {
		t.Fatalf("Body = %q, want the whole file", def.Body)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "never fires") {
		t.Fatalf("warnings = %v, want one never-fires warning", warnings)
	}
}

// A malformed file never fires and still holds its id — the one it declares
// when the header decodes that far, else the one its path gives — so a
// farther layer's definition of that id does not fire in its place.
func TestParseDefinitionMalformedNeverFiresAndHoldsItsID(t *testing.T) {
	cases := map[string]struct{ content, id string }{
		"unterminated frontmatter": {"---\nwhen: [show_backlog]\nno closing delimiter", "bad"},
		"non-mapping frontmatter":  {"---\n- a\n- b\n---\nbody", "bad"},
		"scalar frontmatter":       {"---\nhello\n---\nbody", "bad"},
		"when not a sequence":      {"---\nid: done\nwhen: enter closed\n---\nbody", "done"},
		"when entry a mapping":     {"---\nid: done\nwhen:\n  - {enter: closed}\n---\nbody", "done"},
		"labels not a sequence":    {"---\nlabels: 17\n---\nbody", "bad"},
		"name a list":              {"---\nid: done\nname: [x]\nwhen: [enter closed]\n---\nbody", "done"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			def, warnings := parseDefinition(tc.content, "bad.md", templates.SourceProject)
			if def.ID != tc.id || !def.Activation.Never() {
				t.Fatalf("definition = %+v, want id %q held by one that never fires", def, tc.id)
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

// A when: entry naming no moment this lit fires — an event outside the
// catalog (one a newer lit may fire, or one an older lit fired for a change of
// state), or a state the lifecycle does not have — never fires, with a warning
// saying why; the rest of the list still binds, since entries are
// alternatives.
func TestParseDefinitionUnreadableWhenEntryWarnsAndNeverFires(t *testing.T) {
	cases := map[string]string{
		"warp_drive_engaged": "enter <state> or exit <state>",
		"work_started":       "enter <state> or exit <state>",
		"phase_set":          "enter <state> or exit <state>",
		"work_finished":      "enter <state> or exit <state>",
		"ticket_closed":      "enter <state> or exit <state>",
		"ticket_reopened":    "enter <state> or exit <state>",
		"enter reveiw":       "open, in_progress, plan, impl, review, merge, closed",
		"exit in progress":   "open, in_progress, plan, impl, review, merge, closed",
		"":                   "blank",
	}
	for entry, says := range cases {
		t.Run(entry, func(t *testing.T) {
			def, warnings := parseDefinition("---\nwhen: ['"+entry+"', enter closed]\n---\nbody", "old.md", templates.SourceProject)
			want := firesOn([]Moment{entering("closed")}, nil)
			if !reflect.DeepEqual(def.Activation, want) {
				t.Fatalf("Activation = %+v, want %+v: the readable entry still binds", def.Activation, want)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0].Message, says) {
				t.Fatalf("warnings = %v, want one naming %q", warnings, says)
			}
		})
	}
}

// A key written with no entry lit reads can never hold, so the definition
// never fires: dropping the key instead would widen it to the other key
// alone. That holds for a key written empty as much as for one whose every
// entry is refused.
func TestParseDefinitionKeyWithNoReadableEntryNeverWidens(t *testing.T) {
	cases := map[string]string{
		"when, beside labels": "---\nwhen: [work_finished]\nlabels: [needs-design]\n---\nbody",
		"blank when":          "---\nwhen: ['']\nlabels: [needs-design]\n---\nbody",
		"labels, beside when": "---\nwhen: [enter closed]\nlabels: ['a,b']\n---\nbody",
		"blank labels":        "---\nwhen: [enter closed]\nlabels: ['  ']\n---\nbody",
		"empty labels":        "---\nwhen: [enter closed]\nlabels: []\n---\nbody",
		"bare when":           "---\nwhen:\nlabels: [needs-design]\n---\nbody",
		"empty when":          "---\nwhen: []\nlabels: [needs-design]\n---\nbody",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			def, warnings := parseDefinition(content, "narrow.md", templates.SourceProject)
			if !def.Activation.Never() {
				t.Fatalf("Activation = %+v, want one that never fires", def.Activation)
			}
			if len(warnings) != 1 {
				t.Fatalf("warnings = %v, want one for the unreadable entry", warnings)
			}
		})
	}
}

// Two keys written empty are two keys that can never hold, each warned of —
// not a file with no keys.
func TestParseDefinitionBothKeysWrittenEmptyWarnEach(t *testing.T) {
	def, warnings := parseDefinition("---\nwhen: []\nlabels: []\n---\nbody", "empty.md", templates.SourceProject)
	if !def.Activation.Never() {
		t.Fatalf("Activation = %+v, want one that never fires", def.Activation)
	}
	if len(warnings) != 2 || !strings.HasPrefix(warnings[0].Message, "when: lists no entry") || !strings.HasPrefix(warnings[1].Message, "labels: lists no entry") {
		t.Fatalf("warnings = %v, want one per key written empty", warnings)
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
			def, warnings := parseDefinition(content, "done.md", templates.SourceProject)
			if def.ID != "done" {
				t.Fatalf("ID = %q, want the file's id held", def.ID)
			}
			everything := Occasion{
				Moments: []Moment{EventShowTicket, entering("closed"), exiting("plan")},
				Labels:  []string{"needs-design"},
			}
			if fires(def, everything) {
				t.Fatalf("definition %+v fired; a file lit cannot fully read never fires", def)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "does not read") || !strings.Contains(warnings[0].Message, "when: [enter closed]") {
				t.Fatalf("warnings = %v, want one naming the unread key and the when: spelling", warnings)
			}
		})
	}
}

func TestParseDefinitionNormalization(t *testing.T) {
	content := "---\nid: '  spaced  '\nlabels: ['  A  ']\nwhen: [' Show_Ticket ', '  ENTER   In_Progress ', 'exit In-Progress', 'enter Review', 'SHOW_TICKET']\n---\n\n  body text  \n"
	def, warnings := parseDefinition(content, "n.md", templates.SourceProject)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none — a case or alias variant is not an unknown moment", warnings)
	}
	if def.ID != "spaced" {
		t.Fatalf("ID = %q, want trimmed explicit id", def.ID)
	}
	want := firesOn([]Moment{EventShowTicket, entering("in_progress"), exiting("in_progress"), entering("review")}, []string{"a"})
	if !reflect.DeepEqual(def.Activation, want) {
		t.Fatalf("Activation = %+v, want %+v: lowercased, whitespace collapsed, aliases read as the lifecycle reads them, each moment once", def.Activation, want)
	}
	if def.Body != "body text" {
		t.Fatalf("Body = %q, want trimmed", def.Body)
	}
}

// Labels spelled apart that name one label are one entry, so a ticket
// carrying it gives the definition one reason, not one per spelling.
func TestParseDefinitionReadsARepeatedLabelOnce(t *testing.T) {
	def, warnings := parseDefinition("---\nlabels: [x, X, ' x ']\n---\nbody", "x.md", templates.SourceProject)
	if len(warnings) != 0 || !reflect.DeepEqual(def.Activation.Labels.Entries(), []string{"x"}) {
		t.Fatalf("Labels = %v, warnings %v; want [x] and no warning", def.Activation.Labels.Entries(), warnings)
	}
	if got := def.MatchReasons(Occasion{Moments: []Moment{EventShowTicket}, Labels: []string{"x"}}); !reflect.DeepEqual(got, []string{"label:x"}) {
		t.Fatalf("MatchReasons() = %v, want one label:x", got)
	}
}

// Frontmatter labels are stamped into the store's canonical form at parse
// time, so an authored "Needs-Design" matches the persisted "needs-design".
func TestParseDefinitionLabelsCanonicalized(t *testing.T) {
	def, warnings := parseDefinition("---\nlabels: ['Needs-Design', 'has,comma']\n---\nbody", "l.md", templates.SourceProject)
	if !reflect.DeepEqual(def.Activation.Labels.Entries(), []string{"needs-design"}) {
		t.Fatalf("Labels = %v, want lowercased canonical form", def.Activation.Labels.Entries())
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "has,comma") {
		t.Fatalf("warnings = %v, want one can-never-match warning for the comma label", warnings)
	}
}

// ParseMoment reads every spelling a when: entry takes into the moment it
// names, and refuses, saying why, text naming no moment this lit fires.
func TestParseMoment(t *testing.T) {
	cases := map[string]Moment{
		"enter closed":      entering("closed"),
		"exit plan":         exiting("plan"),
		"Exit  Review":      exiting("review"),
		"enter in-progress": entering("in_progress"),
		"exit live":         exiting("live"),
		"enter Archived":    entering("archived"),
		"show_ticket":       EventShowTicket,
	}
	for text, want := range cases {
		got, err := ParseMoment(text)
		if err != nil || got != want {
			t.Errorf("ParseMoment(%q) = %v, %v; want %v", text, got, err, want)
		}
		if again, _ := ParseMoment(got.String()); again != got {
			t.Errorf("ParseMoment(%q.String()) = %v, want it to read back as itself", got, again)
		}
	}
	refused := map[string]string{
		"  ":                   "blank",
		"enter":                `unknown event "enter"`,
		"work_finished":        `unknown event "work_finished"`,
		"enter deploy:staging": `unknown state "deploy:staging"`,
		"exit strategy":        `unknown state "strategy"`,
	}
	for text, says := range refused {
		if got, err := ParseMoment(text); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("ParseMoment(%q) = %v, %v; want an error naming %s", text, got, err, says)
		}
	}
}

// Every moment this lit fires reads back from its own spelling.
func TestMomentsReadBackAsThemselves(t *testing.T) {
	for _, moment := range Moments() {
		if got, err := ParseMoment(moment.String()); err != nil || got != moment {
			t.Errorf("ParseMoment(%q) = %v, %v; want the moment", moment, got, err)
		}
	}
}

func TestParseDefinitionCRLF(t *testing.T) {
	def, _ := parseDefinition("---\r\nwhen: [show_backlog]\r\n---\r\nbody\r\n", "crlf.md", templates.SourceProject)
	if !reflect.DeepEqual(def.Activation.When.Entries(), []Moment{EventShowBacklog}) {
		t.Fatalf("When = %v, want [show_backlog]", def.Activation.When.Entries())
	}
}

// Delimiter lines tolerate trailing horizontal whitespace, per the frontmatter
// convention — a trailing space must not silently degrade the file to one
// that never fires.
func TestParseDefinitionDelimiterTrailingWhitespace(t *testing.T) {
	def, warnings := parseDefinition("--- \nwhen: [show_backlog]\n---\t\nbody", "ws.md", templates.SourceProject)
	if !reflect.DeepEqual(def.Activation.When.Entries(), []Moment{EventShowBacklog}) {
		t.Fatalf("When = %v, want frontmatter recognized despite trailing whitespace", def.Activation.When.Entries())
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}
