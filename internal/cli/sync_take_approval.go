package cli

import (
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

// ownerApprovalRefusalError makes the store's take gate returnable from the
// command: its Error() is the full agent-facing refusal block, so the top-level
// error sink prints it exactly as SyncFailureError's block prints, and ExitCode
// maps it to the conflict exit — the divergence is still unresolved, which is
// what that exit already means. [LAW:single-enforcer] one value, one rendering.
type ownerApprovalRefusalError struct {
	Approval       store.OwnerApprovalRequiredError
	Remote, Branch string
}

func (e ownerApprovalRefusalError) Error() string {
	return e.blockString()
}

// blockString renders the take refusal in the sync-failure block's four-part
// shape — directive, what would happen, how to proceed, binding — because the
// agent reading it has been trained by that contract's structure everywhere
// else divergence surfaces. The same operations run every call; the approval's
// values (side, inventory, staleness) decide the content.
// [LAW:dataflow-not-control-flow]
func (e ownerApprovalRefusalError) blockString() string {
	kept, dropped := takeSideEffects(e.Approval.Choice)
	var b strings.Builder
	b.WriteString("<agent-instructions>\n")
	fmt.Fprintf(&b, "lit sync reconcile take %s is DESTRUCTIVE and did not run: it needs the owner's explicit approval.\n\n", e.Approval.Choice)

	fmt.Fprintf(&b,
		"WHAT IT WOULD DO: keep the %s backlog wholesale and permanently discard every issue only the %s side holds — %s.\n\n",
		kept, dropped, describeIDSet(discardedIDs(e.Approval.Inventory, e.Approval.Choice)))

	failure := SyncFailure{Inventory: e.Approval.Inventory}
	for _, line := range failure.inventoryLines() {
		fmt.Fprintf(&b, "%s\n", line)
	}

	b.WriteString("WHY YOU ARE BLOCKED: choosing which side of a forked backlog survives is the OWNER's decision — the human whose work one side carries — never an agent's to make alone. Do not self-approve; approval asserted without the owner's explicit instruction is a false claim.\n\n")

	b.WriteString("HOW TO PROCEED (in order):\n")
	fmt.Fprintf(&b, "  1. lit sync reconcile combine   # NO approval needed: union both backlogs, keeping every issue — prefer this unless the owner wants a side gone\n")
	fmt.Fprintf(&b, "  2. Surface this fork to the owner, including WHAT EACH SIDE HOLDS above (they may also have been notified out-of-band)\n")
	fmt.Fprintf(&b, "  3. Only with the owner's explicit choice to discard the %s side:\n", dropped)
	fmt.Fprintf(&b, "     lit sync reconcile take %s --owner-approved %s\n\n", e.Approval.Choice, e.Approval.ApprovalToken)

	fmt.Fprintf(&b, "%s\n", e.bindingLine())
	b.WriteString("</agent-instructions>")
	return b.String()
}

// bindingLine states what the token authorizes and, when a supplied token
// missed, why — the divergence moved or the token named the other side — so a
// stale approval reads as "re-read the state and re-approve", never as a
// mysterious rejection. [LAW:no-silent-failure]
func (e ownerApprovalRefusalError) bindingLine() string {
	binding := fmt.Sprintf(
		"The token approves destroying exactly this fork (local %s vs remote %s/%s at %s, side %s); any new commit on either side voids it.",
		shortHead(e.Approval.LocalHead), e.Remote, e.Branch, shortHead(e.Approval.RemoteHead), e.Approval.Choice)
	if e.Approval.Stale {
		return binding + " The token you supplied no longer matches — the backlog moved since it was issued, or it was issued for the other side. Re-read the state above and get fresh owner approval."
	}
	return binding
}

// takeSideEffects names which side a take keeps and which it destroys, so the
// refusal's prose and the trace vocabulary agree on the same pair.
// [LAW:one-source-of-truth]
func takeSideEffects(choice store.UnrelatedResolution) (kept, dropped string) {
	if choice == store.TakeRemote {
		return "remote", "local"
	}
	return "local", "remote"
}

// shortHead abbreviates a commit hash for prose, keeping an absent head an
// explicit unknown rather than an empty gap. [LAW:no-silent-failure]
func shortHead(head string) string {
	switch {
	case head == "":
		return "(unknown)"
	case len(head) > 12:
		return head[:12]
	default:
		return head
	}
}
