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

import (
	"context"
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

// liveCommandNames projects the registry's dispatchable-and-not-retired
// command set. Reading Retired from the spec — never help output, hidden
// flags, or runtime exit codes — is what keeps a hidden-but-live command
// (`lit workflows`) in and a retired-but-dispatchable one (`lit ready`) out.
func liveCommandNames() map[string]bool {
	live := make(map[string]bool)
	for _, spec := range commandSpecs(context.Background(), io.Discard, io.Discard) {
		if !spec.Retired {
			live[spec.Name] = true
		}
	}
	return live
}

// TestShippedTemplatesNameOnlyDispatchedCommands is the gate: it walks every
// managed template's embedded default and refuses any taught `lit <command>`
// whose command is retired or unknown. Reintroducing `lit ready` into any
// template turns this red.
func TestShippedTemplatesNameOnlyDispatchedCommands(t *testing.T) {
	live := liveCommandNames()
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
// an unknown command are both refused, while live and hidden-but-live commands
// and prose mentions of lit pass.
func TestGateRefusesRetiredAndUnknownTokens(t *testing.T) {
	live := liveCommandNames()
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
		{"hidden-but-live is not retired", "t.md", "`lit workflows` — see the lifecycle", nil},
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

// TestRetiredSpecsStayHidden pins the registry-coherence half of the
// retired/hidden distinction: Retired without Hidden would advertise a
// command the templates are forbidden to teach. retiredSpec couples the two;
// this refuses any future row that states one without the other.
func TestRetiredSpecsStayHidden(t *testing.T) {
	for _, spec := range commandSpecs(context.Background(), io.Discard, io.Discard) {
		if spec.Retired && !spec.Hidden {
			t.Errorf("command %q is Retired but not Hidden; retired rows must go through retiredSpec", spec.Name)
		}
		if spec.Retired && !strings.HasPrefix(spec.Summary, "(retired) ") {
			t.Errorf("command %q is Retired but its summary %q does not say so", spec.Name, spec.Summary)
		}
	}
}
