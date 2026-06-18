package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/merge"
)

// proseResolveCommand is the command the agent runs to finalize a prose-pending
// reconcile with its merged text. Named once so the guidance template and any
// future help text cannot drift from the registered subcommand. [LAW:one-source-of-truth]
const proseResolveCommand = "lit sync reconcile resolve"

// guidanceClose is the owner-authored closing instruction printed under every
// prose-pending surface, verbatim. It frames the fallback when the agent cannot
// merge inline: summarize and escalate to the user with enough context to decide
// quickly — lit itself never asks the user to pick a side. [LAW:one-source-of-truth]
const guidanceClose = "If you're able to resolve this inline, please do so. Ensure you follow all user guidance while doing so. If you cannot resolve this inline, please summarize the decision and surface it to the user. Ensure you provide options and context that will allow the user to make the decision quickly and efficiently."

// renderProsePendingGuidance prints the one authoritative prose-pending surface:
// per diverged field, the base/ours/theirs text, framed as a transient state the
// agent resolves inline by MERGING both intents into one coherent text. lit takes
// no side and offers no winner-pick flag — the merged text the agent supplies is
// the whole decision. [LAW:one-source-of-truth] Both the active `lit sync
// reconcile` command and the passive inline nudge render through here, so the
// guidance can never drift between the two surfaces.
//
// The transient nature is load-bearing: the divergence is re-derived live, so an
// agent that reads this, merges, and runs the printed resolve command finalizes
// the reconcile; an agent that does nothing leaves the clone diverged and usable.
func renderProsePendingGuidance(w io.Writer, pending []merge.ProsePending) error {
	ordered := merge.SortPending(pending)
	var b strings.Builder

	b.WriteString("<agent-instructions>\n")
	b.WriteString("A clone of this backlog diverged from the remote. The field-aware merge settled every field EXCEPT the free-text below, which was rewritten on BOTH sides. This is a transient state for you to resolve inline now — local reads still serve the clone's own data, and nothing is committed until you finalize.\n\n")
	b.WriteString("For each field, MERGE 'ours' and 'theirs' into ONE coherent text that preserves BOTH intents. You are not picking a winner — that is exactly why this is yours and not the engine's. 'base' is the common ancestor, shown so you can see what each side changed.\n\n")

	for _, p := range ordered {
		fmt.Fprintf(&b, "── %s · %s ──\n", p.IssueID, p.Field)
		writeProseSection(&b, "base", p.Base)
		writeProseSection(&b, "ours", p.Ours)
		writeProseSection(&b, "theirs", p.Theirs)
		b.WriteString("\n")
	}

	b.WriteString("To finalize, supply your merged text for EVERY field above in ONE command (the divergence is re-derived live, so partial or stale resolutions are rejected and re-surfaced):\n\n")
	b.WriteString("  ")
	b.WriteString(proseResolveCommand)
	for _, p := range ordered {
		fmt.Fprintf(&b, " \\\n    --resolve '%s:%s=<your merged text>'", p.IssueID, p.Field)
	}
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "To leave the clone diverged for now (it stays usable, and a later command re-surfaces this): %s --abort\n\n", proseReconcileAbortHint)

	b.WriteString(guidanceClose)
	b.WriteString("\n</agent-instructions>\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// proseReconcileAbortHint is the abort command shown in the guidance. Kept beside
// proseResolveCommand so both halves of the surface name the same family.
const proseReconcileAbortHint = "lit sync reconcile abort"

// renderProsePendingNudge prints the compact, after-the-fact surface the inline
// auto-reconcile emits: it names exactly which fields diverged and points at the
// full guidance, WITHOUT dumping every base/ours/theirs body over a routine
// command's output. The clone keeps working on local truth; this is a transient
// state the agent resolves when ready, so the nudge never fails the command that
// triggered it. [LAW:no-silent-failure] the divergence is surfaced, not buried in
// a trace. The full base/ours/theirs and the resolve command live behind `lit
// sync reconcile`, rendered by renderProsePendingGuidance — one source of truth.
func renderProsePendingNudge(w io.Writer, pending []merge.ProsePending) error {
	ordered := merge.SortPending(pending)
	var b strings.Builder
	b.WriteString("<agent-instructions>\n")
	fmt.Fprintf(&b, "lit: a clone diverged from the remote on %d free-text field(s) rewritten on both sides — settled everything else, held these for your semantic merge:\n", len(ordered))
	for _, p := range ordered {
		fmt.Fprintf(&b, "  - %s · %s\n", p.IssueID, p.Field)
	}
	fmt.Fprintf(&b, "Run `%s` to see base/ours/theirs and merge them inline (it stays diverged and usable until you do).\n", proseReconcileShowCommand)
	b.WriteString("</agent-instructions>\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// proseReconcileShowCommand is the command that renders the full prose-pending
// guidance. Named once so the nudge and any help text point at the same surface.
const proseReconcileShowCommand = "lit sync reconcile"

// writeProseSection prints one labeled version, making an empty value explicit
// rather than rendering a blank the agent might misread as "missing". [LAW:no-silent-failure]
func writeProseSection(b *strings.Builder, label, text string) {
	if strings.TrimSpace(text) == "" {
		fmt.Fprintf(b, "  %s: (empty)\n", label)
		return
	}
	fmt.Fprintf(b, "  %s: %s\n", label, text)
}

// parseProseResolutions turns the repeated `--resolve ID:FIELD=TEXT` values into
// resolutions. The value is split on the FIRST ':' (issue ids never contain one)
// then the FIRST '=' (field names never contain one), so the remaining TEXT may
// hold any character, including ':' '=' and newlines. A malformed token or an
// unknown field is a usage error, surfaced loudly rather than silently dropped.
// [LAW:no-silent-failure]
func parseProseResolutions(values []string) ([]merge.ProseResolution, error) {
	resolutions := make([]merge.ProseResolution, 0, len(values))
	for _, raw := range values {
		colon := strings.IndexByte(raw, ':')
		if colon <= 0 {
			return nil, UsageError{Message: fmt.Sprintf("invalid --resolve %q: expected ISSUE_ID:FIELD=TEXT", raw)}
		}
		issueID := raw[:colon]
		rest := raw[colon+1:]
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 {
			return nil, UsageError{Message: fmt.Sprintf("invalid --resolve %q: expected ISSUE_ID:FIELD=TEXT", raw)}
		}
		field, err := parseProseField(rest[:eq])
		if err != nil {
			return nil, err
		}
		resolutions = append(resolutions, merge.ProseResolution{
			IssueID: issueID,
			Field:   field,
			Text:    rest[eq+1:],
		})
	}
	return resolutions, nil
}

// parseProseField maps a field token to its ProseField. Only the three free-text
// fields that ever reach this surface are legal; every other field converges
// deterministically in the engine and can never be pending. [LAW:single-enforcer]
func parseProseField(token string) (merge.ProseField, error) {
	switch merge.ProseField(token) {
	case merge.ProseTitle:
		return merge.ProseTitle, nil
	case merge.ProseDescription:
		return merge.ProseDescription, nil
	case merge.ProsePrompt:
		return merge.ProsePrompt, nil
	default:
		return "", UsageError{Message: fmt.Sprintf("unknown reconcile field %q: expected one of %s, %s, %s", token, merge.ProseTitle, merge.ProseDescription, merge.ProsePrompt)}
	}
}
