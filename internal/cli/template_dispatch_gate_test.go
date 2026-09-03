package cli

// The template dispatch gate: no shipped template may name a command lit does
// not dispatch as a live command. Both halves of the check are derived, never
// hand-maintained — the template list from templates.Names() and the live
// command set from the commandSpecs registry — so a command retirement and the
// text that teaches the command are reconciled at build time in the one repo
// that owns both. [LAW:one-source-of-truth]
//
// The incident this guards against: `lit ready` was retired but the shipped
// /next skill kept teaching it. Retired commands still dispatch (they print a
// pointer and exit 3), so any predicate of the form "does this token resolve?"
// passes the exact string that caused the incident. The predicate here is
// "resolves to a spec the registry marks live", read from CommandSpec.Retired.
//
// Scope is top-level command names. Retirement one level down (`lit bulk
// import` — a family row marked only hidden, with a pointer runner) is not
// yet legible as data in the family tables; making it so and extending the
// gate to second tokens is links-workflow-mrb6.

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/templates"
)

// litToken matches one taught invocation and captures its command word.
// Placeholders (`lit <id>`) and a bare `lit` match nothing: the capture
// requires a command-shaped word.
var litToken = regexp.MustCompile(`\blit\s+([a-z][a-z0-9_-]*)`)

// inlineCodeSpan delimits markdown inline code. The shipped markdown templates
// carry no fenced blocks, so single-backtick spans are the whole code surface.
var inlineCodeSpan = regexp.MustCompile("`[^`]+`")

// undispatchedLitTokens returns every `lit <token>` invocation in one shipped
// template whose token the registry does not dispatch as a live command.
//
// What counts as an invocation is the template language's own quoting, not a
// guess over prose: in markdown only inline code spans are scanned (prose like
// "when lit errors" or "lit tracks tickets" never reaches the token match),
// while a shell template is scanned whole — it is code, and even its message
// strings quote real invocations. [LAW:dataflow-not-control-flow] the file
// extension selects the scan region; the token rule is the same everywhere.
func undispatchedLitTokens(templateName string, content []byte, live map[string]bool) []string {
	regions := []string{string(content)}
	if filepath.Ext(templateName) == ".md" {
		regions = inlineCodeSpan.FindAllString(string(content), -1)
	}
	var bad []string
	for _, region := range regions {
		for _, m := range litToken.FindAllStringSubmatch(region, -1) {
			if token := m[1]; !live[token] && !slices.Contains(bad, token) {
				bad = append(bad, token)
			}
		}
	}
	return bad
}

// liveCommandNames projects a spec list's dispatchable-and-not-retired
// command set. The predicate reads Retired — never Hidden, help output, or
// runtime exit codes — so a hidden-but-live spec stays teachable while a
// retired-but-dispatchable one (`lit ready`) is out. A pure function over the
// specs, so the Retired-vs-Hidden distinction is testable with synthetic rows:
// today every Hidden row in the real registry is also Retired, and only a
// synthetic row can catch a filter that conflates the two fields.
func liveCommandNames(specs []CommandSpec) map[string]bool {
	live := make(map[string]bool)
	for _, spec := range specs {
		if !spec.Retired {
			live[spec.Name] = true
		}
	}
	return live
}

// registryCommandNames is liveCommandNames over the real registry.
func registryCommandNames() map[string]bool {
	return liveCommandNames(commandSpecs(context.Background(), io.Discard, io.Discard))
}

// TestLiveCommandSetReadsRetiredNotHidden pins the field the projection reads.
// The real registry cannot pin it — its Hidden and Retired rows currently
// coincide — so a synthetic hidden-but-live row does: a future filter on
// Hidden instead of Retired drops that row and fails here.
func TestLiveCommandSetReadsRetiredNotHidden(t *testing.T) {
	live := liveCommandNames([]CommandSpec{
		{Name: "visible-live"},
		{Name: "hidden-live", Hidden: true},
		{Name: "retired", Hidden: true, Retired: true},
	})
	if !live["visible-live"] || !live["hidden-live"] {
		t.Errorf("live set %v must keep visible-live and hidden-live: Hidden is not Retired", live)
	}
	if live["retired"] {
		t.Errorf("live set %v must drop the retired row", live)
	}
}

// TestShippedTemplatesNameOnlyDispatchedCommands is the gate: it walks every
// managed template's embedded default and refuses any taught `lit <command>`
// whose command is retired or unknown. Reintroducing `lit ready` into any
// template turns this red.
func TestShippedTemplatesNameOnlyDispatchedCommands(t *testing.T) {
	live := registryCommandNames()
	for _, name := range templates.Names() {
		content, err := templates.EmbeddedDefault(name)
		if err != nil {
			t.Fatalf("EmbeddedDefault(%q): %v", name, err)
		}
		for _, token := range undispatchedLitTokens(name, content, live) {
			t.Errorf("template %s teaches `lit %s`, which lit does not dispatch as a live command (retired or unknown); update the template or the registry in the same change", name, token)
		}
	}
}

// TestGateRefusesRetiredAndUnknownTokens proves the gate can fail, on the same
// code path the gate runs, before anyone needs to mutate a shipped template:
// the retired `lit ready` — the incident string, which still dispatches — and
// an unknown command are both refused, while live commands and prose mentions
// of lit pass.
func TestGateRefusesRetiredAndUnknownTokens(t *testing.T) {
	live := registryCommandNames()
	cases := []struct {
		name     string
		template string
		content  string
		want     []string
	}{
		{"retired command in markdown span", "t.md", "Take a look at the backlog (`lit ready`).", []string{"ready"}},
		{"unknown command in markdown span", "t.md", "Run `lit frobnicate` first.", []string{"frobnicate"}},
		{"retired command in shell text", "t.sh", "lit ready >/dev/null", []string{"ready"}},
		{"live command", "t.md", "Run `lit next` and start the ticket.", nil},
		{"live command with subcommand and flags", "t.md", "`lit quickstart work` then `lit sync push --remote origin`", nil},
		{"guidance command taught by the quickstart", "t.md", "`lit workflows` — see the lifecycle", nil},
		{"prose mention outside code span", "t.md", "use when lit errors or lit tracks something", nil},
		{"placeholder and bare lit", "t.md", "`lit <id>` or just `lit`", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := undispatchedLitTokens(tc.template, []byte(tc.content), live)
			if !slices.Equal(got, tc.want) {
				t.Errorf("undispatchedLitTokens(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// TestRetiredSpecsStayCoherent holds every Retired row to all three
// retirement facets. The type cannot: CommandSpec's fields are open to any
// same-package literal, so a row could state Retired without Hidden
// (advertising a command templates may not teach) or without the pointer
// runner (dispatching real work from a "retired" name). retiredSpec is the
// authoring path; this test is the enforcement, and it asserts the runner's
// behavior — the retirement error naming the command — not which closure the
// row happens to hold. [LAW:behavior-not-structure]
func TestRetiredSpecsStayCoherent(t *testing.T) {
	for _, spec := range commandSpecs(context.Background(), io.Discard, io.Discard) {
		if !spec.Retired {
			continue
		}
		if !spec.Hidden {
			t.Errorf("command %q is Retired but not Hidden; retired rows must go through retiredSpec", spec.Name)
		}
		if !strings.HasPrefix(spec.Summary, "(retired) ") {
			t.Errorf("command %q is Retired but its summary %q does not say so", spec.Name, spec.Summary)
		}
		var retired RetiredCommandError
		if err := spec.Run(nil); !errors.As(err, &retired) || retired.Command != spec.Name {
			t.Errorf("command %q is Retired but its runner answered %v, want a RetiredCommandError naming it", spec.Name, err)
		}
	}
}
