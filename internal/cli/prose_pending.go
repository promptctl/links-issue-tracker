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
//
// buildNote is the dev-vs-release build status (resolveBuildStatusNote,
// resolved by the caller), embedded in the envelope in the same position
// SyncFailure.blockString() uses — a prose-pending block is one of the sync
// failure classes, so it names the same diagnostic every other failure surface
// does, even though it renders through this separate path rather than through
// SyncFailure itself. Empty is silently omitted, never fabricated.
func renderProsePendingGuidance(w io.Writer, pending []merge.ProsePending, buildNote string) error {
	ordered := merge.SortPending(pending)
	var b strings.Builder

	b.WriteString(agentInstructionsOpen + "\n")
	b.WriteString("A clone of this backlog diverged from the remote. The field-aware merge settled every field except the free-text below, which was rewritten on both sides. This is a transient state you can resolve inline now — local reads still serve the clone's own data, and nothing is committed until you finalize.\n\n")
	b.WriteString("For each field, merge 'ours' and 'theirs' into one coherent text that preserves both intents. You are not picking a winner — that is exactly why this is yours to merge and not the engine's. 'base' is the common ancestor, shown so you can see what each side changed.\n\n")
	if buildNote != "" {
		fmt.Fprintf(&b, "%s\n\n", buildNote)
	}

	// The id and the three texts are ticket data — base and theirs authored on a
	// machine this one does not control, and no ingest boundary constrains the id —
	// so each reaches the envelope only through quoteRemote. [LAW:single-enforcer]
	for _, p := range ordered {
		fmt.Fprintf(&b, "── %s · %s · fingerprint %s ──\n", quoteRemote(p.IssueID).inline(), p.Field, p.Fingerprint())
		writeProseSection(&b, "base", p.Base)
		writeProseSection(&b, "ours", p.Ours)
		writeProseSection(&b, "theirs", p.Theirs)
		b.WriteString("\n")
	}

	b.WriteString("To finalize, supply your merged text for every field above in one command, each under the fingerprint in its heading (the divergence is re-derived live, so partial or stale resolutions are rejected and re-surfaced — the fingerprint pins your merge to that exact conflict):\n\n")
	b.WriteString("  ")
	b.WriteString(proseResolveCommand)
	// Each field is addressed by its fingerprint alone — hex lit computed — so
	// nothing a remote wrote reaches a line the agent runs in a shell.
	for _, p := range ordered {
		fmt.Fprintf(&b, " \\\n    --resolve '%s=<your merged text>'", p.Fingerprint())
	}
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "To leave the clone diverged for now (it stays usable, and a later command re-surfaces this): %s\n\n", proseReconcileAbortHint)

	b.WriteString(guidanceClose)
	b.WriteString("\n" + agentInstructionsClose + "\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// proseReconcileAbortHint is the abort command shown in the guidance. Kept beside
// proseResolveCommand so both halves of the surface name the same family.
const proseReconcileAbortHint = "lit sync reconcile abort"

// The compact after-the-fact surface the inline auto-reconcile once emitted now
// routes through the one sync-failure contract (SyncFailure/blockString) instead
// of a bespoke nudge, so the held-conflict surface carries the same MUST-NOT-IGNORE
// directive and escalation as every other sync failure. [LAW:single-enforcer] This
// file keeps only renderProsePendingGuidance — the deep base/ours/theirs workbench
// the compact contract points at.

// writeProseSection prints one labeled version as a quoted span, making an empty
// value explicit rather than rendering a blank the agent might misread as
// "missing". [LAW:no-silent-failure]
func writeProseSection(b *strings.Builder, label, text string) {
	if strings.TrimSpace(text) == "" {
		fmt.Fprintf(b, "  %s: (empty)\n", label)
		return
	}
	fmt.Fprintf(b, "  %s:\n", label)
	for _, line := range quoteRemote(text).fenced("    ") {
		fmt.Fprintf(b, "%s\n", line)
	}
}

// parseProseResolutions turns the repeated `--resolve FINGERPRINT=TEXT` values
// into resolutions. The prefix before the FIRST '=' is the conflict fingerprint,
// which is hex and never holds '=', so the remaining TEXT may hold any character,
// including '=' and newlines. A prefix that is not a fingerprint — the retired
// ID:FIELD:FINGERPRINT form among them — is a usage error, surfaced loudly rather
// than sent on to match no conflict and read as a divergence that changed.
// [LAW:no-silent-failure]
func parseProseResolutions(values []string) ([]merge.ProseResolution, error) {
	const shape = "expected FINGERPRINT=TEXT (copy the fingerprint from `lit sync reconcile`)"
	resolutions := make([]merge.ProseResolution, 0, len(values))
	for _, raw := range values {
		prefix, text, found := strings.Cut(raw, "=")
		fingerprint, ok := merge.ParseFingerprint(prefix)
		if !found || !ok {
			return nil, UsageError{Message: fmt.Sprintf("invalid --resolve %q: %s", raw, shape)}
		}
		resolutions = append(resolutions, merge.ProseResolution{Fingerprint: fingerprint, Text: text})
	}
	return resolutions, nil
}
