package cli

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/docclaims"
)

// nextSkillPath is the shipped /next skill, relative to the repository root.
const nextSkillPath = "claude-plugin/skills/next/SKILL.md"

// emptyHandedOpener is what most empty-handed answers from lit next open with.
// Scanning the shipped literals for it finds every branch of their renderers,
// including one no sample in sealedNextOutcomes reaches.
const emptyHandedOpener = "no ready work"

// The /next skill routes on which empty-handed answer lit next gave, quoting
// each one's lead. The leads are read off what ships rather than listed here,
// so rewording one or adding one fails until the skill says what it means.
// They are found two ways: every shipped literal that opens with the opener,
// and every outcome in the sealed sum that hands no row back, rendered — so an
// answer with other opening words is held too. [LAW:one-source-of-truth] the
// shipped text is the fact and the skill is a copy, so the two are compared in
// both directions: every lead the product ships is quoted, and every quotation
// still renders a shipped lead.
func TestNextSkillQuotesEveryEmptyHandedLead(t *testing.T) {
	root := mustRepoRoot(t)
	corpus, err := docclaims.ShippedText(os.DirFS(root))
	if err != nil {
		t.Fatalf("ShippedText: %v", err)
	}
	var shippedLeads []string
	// answers maps each empty-handed lead to the shipped text it was read off.
	answers := map[string]string{}
	for handle := range corpus {
		lead := literalLead(handle)
		shippedLeads = append(shippedLeads, lead)
		if !strings.HasPrefix(handle, emptyHandedOpener) {
			continue
		}
		// The skill tells the answers apart by their leads, so two that share
		// one are two answers the skill cannot tell apart.
		if other, shared := answers[lead]; shared {
			t.Errorf("%q and %q both open with `%s`, so %s cannot tell them apart — give one its own opening words", other, handle, lead, nextSkillPath)
		}
		answers[lead] = handle
	}
	// [LAW:no-silent-failure] a scan that finds nothing would pass every check
	// below over an empty list.
	if len(answers) == 0 {
		t.Fatalf("no shipped literal opens with %q, so the scan for lit next's empty-handed answers is broken", emptyHandedOpener)
	}
	for _, tc := range sealedNextOutcomes {
		if _, served := servedRow(tc.outcome); served {
			continue
		}
		answer, ok := tc.outcome.(error)
		if !ok {
			t.Errorf("%T hands no row back and is not an error, so lit next has no way to say it", tc.outcome)
			continue
		}
		rendered := answer.Error()
		lead, ok := openingLead(rendered, shippedLeads)
		if !ok {
			t.Errorf("%T renders %q, which opens with no shipped literal's fixed text", tc.outcome, rendered)
			continue
		}
		answers[lead] = rendered
	}
	leads := slices.Collect(maps.Keys(answers))
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

// literalLead is a literal's fixed opening: its text before the first format
// verb, without the separator that introduces what the verb fills in.
func literalLead(literal string) string {
	if i := strings.Index(literal, "%"); i >= 0 {
		literal = literal[:i]
	}
	return strings.TrimRight(literal, ": ")
}

// openingLead is the longest shipped lead a rendered answer opens with: the
// fixed text of the format string that rendered it. Longest, because shorter
// leads of other literals open it too.
func openingLead(rendered string, shippedLeads []string) (string, bool) {
	best := ""
	for _, lead := range shippedLeads {
		if strings.HasPrefix(rendered, lead) && len(lead) > len(best) {
			best = lead
		}
	}
	return best, best != ""
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
