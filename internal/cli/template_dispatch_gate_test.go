package cli

// The template dispatch gate: no shipped template may name a command lit does
// not dispatch as a live command. Both halves of the check are derived, never
// hand-maintained — the template list from templates.Names() and the live
// command set from the commandSpecs registry — so a command retirement and the
// text that teaches the command are reconciled at build time in the one repo
// that owns both. [LAW:one-source-of-truth]
//
// Retired commands still dispatch (they print a pointer and exit 3), so any
// predicate of the form "does this token resolve?" passes a retired command.
// The predicate here is "resolves to a spec the registry marks live", read
// from CommandSpec.Retired, and "names no subcommand path the registry marks
// retired", read from SubcommandSpec.Retired at any depth (`lit bulk import`).
//
// Below the top level only a retired path is refused, never an unknown word: a
// family's next word may be a positional rather than a subcommand (`lit rank
// <id>` takes an id there), so the tree can say what is withdrawn but not that
// a word is unknown.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/docclaims"
	"github.com/promptctl/links-issue-tracker/internal/templates"
)

// litToken matches one taught invocation and captures its run of
// command-shaped words on that line: `lit bulk import --path x` captures
// "bulk import". Placeholders (`lit <id>`) and a bare `lit` match nothing: the
// capture requires a command-shaped first word.
var litToken = regexp.MustCompile(`\blit\s+([a-z][a-z0-9_-]*(?:[ \t]+[a-z][a-z0-9_-]*)*)`)

// undispatchedLitTokens returns every command path taught in one shipped
// template that the surface refuses: a first word that is not a live command,
// or a longer path that names a retired subcommand.
//
// What counts as an invocation is the template language's own quoting, not a
// guess over prose: in markdown only inline code spans are scanned (prose like
// "when lit errors" or "lit tracks tickets" never reaches the token match),
// read by the one markdown span reader the doc gates share, while a shell
// template is scanned whole — it is code, and even its message strings quote
// real invocations. [LAW:dataflow-not-control-flow] the file extension selects
// the scan region; the token rule is the same everywhere.
// [LAW:single-enforcer] a code span is what docclaims.CodeSpans says it is.
func undispatchedLitTokens(templateName string, content []byte, surface taughtSurface) ([]string, error) {
	regions := []string{string(content)}
	if filepath.Ext(templateName) == ".md" {
		spans, err := docclaims.CodeSpans(string(content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", templateName, err)
		}
		regions = regions[:0]
		for _, s := range spans {
			regions = append(regions, s.Text)
		}
	}
	var bad []string
	for _, region := range regions {
		for _, m := range litToken.FindAllStringSubmatch(region, -1) {
			for _, words := range invocations(m[1]) {
				if path, refused := surface.refusedPath(words); refused && !slices.Contains(bad, path) {
					bad = append(bad, path)
				}
			}
		}
	}
	return bad, nil
}

// invocations splits one captured word run at each later `lit`: the run of
// `lit next then lit ready` is two taught invocations, and read as one it would
// hide the retired `ready` behind the live `next`. A `lit` followed by no
// command-shaped word teaches nothing, as for litToken itself.
func invocations(run string) [][]string {
	var out [][]string
	var words []string
	for _, w := range append(strings.Fields(run), "lit") {
		if w != "lit" {
			words = append(words, w)
			continue
		}
		if len(words) > 0 {
			out = append(out, words)
		}
		words = nil
	}
	return out
}

// taughtSurface is what a template may name, as the registry states it: the
// top-level commands that are not Retired, and the space-joined subcommand
// paths that are.
type taughtSurface struct {
	live    map[string]bool
	retired map[string]bool
}

// refusedPath returns the leading part of a taught word run that the surface
// refuses, if any part is refused.
func (s taughtSurface) refusedPath(words []string) (string, bool) {
	if !s.live[words[0]] {
		return words[0], true
	}
	for k := 2; k <= len(words); k++ {
		if path := strings.Join(words[:k], " "); s.retired[path] {
			return path, true
		}
	}
	return "", false
}

// taughtSurfaceOf projects a spec list's surface. The predicate reads Retired —
// never Hidden, help output, or runtime exit codes — at every depth, so a
// hidden-but-live command or subcommand stays teachable while a
// retired-but-dispatchable one (`lit ready`, `lit bulk import`) is out. A pure
// function over the specs, so the Retired-vs-Hidden distinction is testable
// with synthetic rows: the real registry's hidden-but-live subcommands
// (`sync __mirror-bg`) are not words litToken can match, and every Hidden
// top-level row is also Retired, so only a synthetic row can catch a filter
// that conflates the two fields.
func taughtSurfaceOf(specs []CommandSpec) taughtSurface {
	surface := taughtSurface{live: map[string]bool{}, retired: map[string]bool{}}
	var walk func(prefix string, subs []SubcommandSpec)
	walk = func(prefix string, subs []SubcommandSpec) {
		for _, sub := range subs {
			path := prefix + " " + sub.Name
			surface.retired[path] = sub.Retired
			walk(path, sub.Subcommands)
		}
	}
	for _, spec := range specs {
		surface.live[spec.Name] = !spec.Retired
		walk(spec.Name, spec.Subcommands)
	}
	return surface
}

// registrySurface is taughtSurfaceOf over the real registry.
func registrySurface() taughtSurface {
	return taughtSurfaceOf(commandSpecs(context.Background(), io.Discard, io.Discard))
}

// TestTaughtSurfaceReadsRetiredNotHidden pins the field the projection reads,
// at the top level and below it. The real registry cannot pin it, so synthetic
// hidden-but-live rows do: a future filter on Hidden instead of Retired refuses
// one of them and fails here.
func TestTaughtSurfaceReadsRetiredNotHidden(t *testing.T) {
	surface := taughtSurfaceOf([]CommandSpec{
		{Name: "visible-live"},
		{Name: "hidden-live", Hidden: true},
		{Name: "retired", Hidden: true, Retired: true},
		{Name: "family", Subcommands: []SubcommandSpec{
			{Name: "live"},
			{Name: "hidden-live", Hidden: true},
			{Name: "retired", Hidden: true, Retired: true},
			{Name: "group", Subcommands: []SubcommandSpec{
				{Name: "retired", Hidden: true, Retired: true},
			}},
		}},
	})
	cases := []struct {
		content string
		want    []string
	}{
		{"`lit visible-live` `lit hidden-live`", nil},
		{"`lit retired`", []string{"retired"}},
		{"`lit family live` `lit family hidden-live` `lit family group`", nil},
		{"`lit family retired --flag`", []string{"family retired"}},
		{"`lit family group retired`", []string{"family group retired"}},
	}
	for _, tc := range cases {
		got, err := undispatchedLitTokens("t.md", []byte(tc.content), surface)
		if err != nil {
			t.Fatalf("undispatchedLitTokens(%q): %v", tc.content, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("undispatchedLitTokens(%q) = %v, want %v", tc.content, got, tc.want)
		}
	}
}

// TestShippedTemplatesNameOnlyDispatchedCommands is the gate: it walks every
// managed template's embedded default and refuses any taught `lit <command>`
// whose command is retired or unknown. Reintroducing `lit ready` into any
// template turns this red.
func TestShippedTemplatesNameOnlyDispatchedCommands(t *testing.T) {
	surface := registrySurface()
	for _, name := range templates.Names() {
		content, err := templates.EmbeddedDefault(name)
		if err != nil {
			t.Fatalf("EmbeddedDefault(%q): %v", name, err)
		}
		tokens, err := undispatchedLitTokens(name, content, surface)
		if err != nil {
			t.Fatalf("undispatchedLitTokens: %v", err)
		}
		for _, token := range tokens {
			t.Errorf("template %s teaches `lit %s`, which lit does not dispatch as a live command (retired or unknown); update the template or the registry in the same change", name, token)
		}
	}
}

// TestGateRefusesRetiredAndUnknownTokens proves the gate can fail, on the same
// code path the gate runs, before anyone needs to mutate a shipped template:
// the retired `lit ready` and `lit bulk import`, which still dispatch, and an
// unknown command are refused, while live commands and subcommands, a
// positional where a family takes one, and prose mentions of lit pass.
func TestGateRefusesRetiredAndUnknownTokens(t *testing.T) {
	surface := registrySurface()
	cases := []struct {
		name     string
		template string
		content  string
		want     []string
	}{
		{"retired command in markdown span", "t.md", "Take a look at the backlog (`lit ready`).", []string{"ready"}},
		{"unknown command in markdown span", "t.md", "Run `lit frobnicate` first.", []string{"frobnicate"}},
		{"retired command in shell text", "t.sh", "lit ready >/dev/null", []string{"ready"}},
		{"retired subcommand in markdown span", "t.md", "Restore with `lit bulk import --path x.json`.", []string{"bulk import"}},
		{"retired subcommand in shell text", "t.sh", "lit bulk import --path x.json || exit 1", []string{"bulk import"}},
		{"live subcommand of the same family", "t.md", "`lit bulk close --ids a,b`", nil},
		{"positional where a family takes one", "t.md", "`lit rank links-abc --top`", nil},
		{"retired command after a live one in one span", "t.md", "`lit next then lit ready`", []string{"ready"}},
		{"retired subcommand after a live one in shell text", "t.sh", "printf 'run lit sync push or lit bulk import'", []string{"bulk import"}},
		{"trailing lit before a placeholder", "t.md", "`lit next then lit <id>`", nil},
		{"live command", "t.md", "Run `lit next` and start the ticket.", nil},
		{"live command with subcommand and flags", "t.md", "`lit quickstart work` then `lit sync push --remote origin`", nil},
		{"guidance command taught by the quickstart", "t.md", "`lit workflows` — see the lifecycle", nil},
		{"prose mention outside code span", "t.md", "use when lit errors or lit tracks something", nil},
		{"placeholder and bare lit", "t.md", "`lit <id>` or just `lit`", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := undispatchedLitTokens(tc.template, []byte(tc.content), surface)
			if err != nil {
				t.Fatalf("undispatchedLitTokens(%q): %v", tc.content, err)
			}
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
