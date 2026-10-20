package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/docclaims"
)

// nextSkillPath is the shipped /next skill, relative to the repository root.
const nextSkillPath = "claude-plugin/skills/next/SKILL.md"

// emptyHandedOpener is what every empty-handed answer from lit next opens
// with, and the skill tells an agent to read the words straight after it. That
// shared opening is how the test finds the answers in what ships.
const emptyHandedOpener = "no ready work"

// The /next skill routes on which empty-handed answer lit next gave, quoting
// each one's lead. The leads are read off the shipped Go literals rather than
// listed here, so rewording one or adding one fails until the skill says what
// it means. [LAW:one-source-of-truth] the shipped text is the fact and the
// skill is a copy, so the two are compared in both directions: every lead the
// product ships is quoted, and every quotation still renders a shipped lead.
func TestNextSkillQuotesEveryEmptyHandedLead(t *testing.T) {
	root := mustRepoRoot(t)
	corpus, err := docclaims.ShippedText(os.DirFS(root))
	if err != nil {
		t.Fatalf("ShippedText: %v", err)
	}
	var leads []string
	for handle := range corpus {
		if strings.HasPrefix(handle, emptyHandedOpener) {
			leads = append(leads, emptyHandedLead(handle))
		}
	}
	// [LAW:no-silent-failure] a scan that finds nothing would pass every check
	// below over an empty list.
	if len(leads) == 0 {
		t.Fatalf("no shipped literal opens with %q, so the scan for lit next's empty-handed answers is broken", emptyHandedOpener)
	}
	spans := nextSkillSpans(t, root)

	for _, lead := range leads {
		if !slices.Contains(spans, lead) {
			t.Errorf("%s does not quote `%s`, which lit next ships as the lead of an empty-handed answer — say in the skill what that answer means", nextSkillPath, lead)
		}
	}
	for _, span := range spans {
		if !strings.HasPrefix(span, emptyHandedOpener) {
			continue
		}
		if lead, ok := renderedLead(span, leads); !ok {
			t.Errorf("%s quotes `%s`, which no empty-handed answer lit next ships opens with — the skill describes an answer that is gone", nextSkillPath, span)
		} else if lead == emptyHandedOpener && span != lead {
			t.Errorf("%s quotes `%s`, which extends only the bare `%s` — no shipped lead renders it", nextSkillPath, span, emptyHandedOpener)
		}
	}
}

// The skill names the two scopes a scoped answer can report, as scope renders
// them, so its description of `no ready work in` stays true to what follows.
func TestNextSkillQuotesBothScopeForms(t *testing.T) {
	spans := nextSkillSpans(t, mustRepoRoot(t))
	for _, form := range []string{Exhausted{}.scope(), Exhausted{Epics: []string{"<ids>"}}.scope()} {
		if !slices.Contains(spans, form) {
			t.Errorf("%s does not quote `%s`, a scope lit next's scoped answer names", nextSkillPath, form)
		}
	}
}

// emptyHandedLead is an answer's fixed opening: its text before the first
// format verb, without the separator that introduces what the verb fills in.
func emptyHandedLead(literal string) string {
	if i := strings.Index(literal, "%"); i >= 0 {
		literal = literal[:i]
	}
	return strings.TrimRight(literal, ": ")
}

// renderedLead is the longest lead a skill span renders: the span itself, or
// the span's opening words up to a space. Longest, because the bare opener is a
// prefix of every lead and would otherwise vouch for any span at all.
func renderedLead(span string, leads []string) (string, bool) {
	best, found := "", false
	for _, lead := range leads {
		if (span == lead || strings.HasPrefix(span, lead+" ")) && len(lead) > len(best) {
			best, found = lead, true
		}
	}
	return best, found
}

func nextSkillSpans(t *testing.T, root string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(nextSkillPath)))
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", nextSkillPath, err)
	}
	spans, err := docclaims.CodeSpans(string(raw))
	if err != nil {
		t.Fatalf("CodeSpans(%s): %v", nextSkillPath, err)
	}
	texts := make([]string, len(spans))
	for i, s := range spans {
		texts[i] = s.Text
	}
	return texts
}
