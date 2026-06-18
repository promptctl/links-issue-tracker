package merge

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Fingerprint identifies the EXACT three-way conflict this pending field
// represents — a digest of base/ours/theirs. The agent merges against a specific
// (base, ours, theirs); if any of those changed before it finalizes (the remote
// advanced, or a local edit landed), the fingerprint changes, so a merged text
// produced for the old conflict no longer matches and must not be committed.
// [LAW:types-are-the-program] the fingerprint makes "this text was merged against
// THIS conflict" a checkable value rather than an assumption.
func (p ProsePending) Fingerprint() string {
	sum := sha256.Sum256([]byte(string(p.Field) + "\x00" + p.Base + "\x00" + p.Ours + "\x00" + p.Theirs))
	return hex.EncodeToString(sum[:6])
}

// ProseResolution is the calling agent's semantic merge of one prose field that
// diverged on both sides: the single coherent Text that preserves BOTH the Ours
// and Theirs intent. It is the enactment of the one judgment step the engine
// deliberately refuses to make. [LAW:decomposition] The agent owns the decision
// (the Text); this package owns only where that text lands in the export.
//
// Fingerprint is the digest of the conflict the agent merged against, copied from
// the pending field's guidance. It is compared to the LIVE conflict's fingerprint
// before the text is spliced, so a merge produced for a since-changed conflict is
// rejected rather than silently applied. [LAW:no-silent-failure]
type ProseResolution struct {
	IssueID     string
	Field       ProseField
	Fingerprint string
	Text        string
}

// proseKey identifies one prose field of one issue — the unit a ProsePending and
// a ProseResolution must agree on. [LAW:types-are-the-program] Making the pairing
// a comparable key lets the bijection check be a set comparison, not field-by-
// field prose.
type proseKey struct {
	IssueID string
	Field   ProseField
}

// ApplyProseResolutions turns a prose-pending merge into a fully settled export
// by splicing the agent's merged text into the provisional rows — but ONLY when
// the supplied resolutions are an exact bijection with the live pending set. A
// resolution missing for a pending field, or a resolution for a field that is no
// longer pending, returns ok=false: the caller re-derives and re-surfaces the
// CURRENT divergence rather than committing against a stale picture.
// [LAW:no-silent-failure] An incomplete or mismatched resolution never produces a
// committable export, so a provisional prose value can never be published by
// omission, and the agent can never silently overwrite a field whose divergence
// changed underneath it.
//
// It is pure: the live pending set comes from the MergeResult, the merged text
// from the agent — no IO, no clock. [LAW:effects-at-boundaries]
func ApplyProseResolutions(result MergeResult, resolutions []ProseResolution) (model.Export, bool) {
	// Each pending field carries the fingerprint of its LIVE conflict, so a
	// resolution must match both the key (this field is pending) AND the
	// fingerprint (it was merged against THIS conflict, not a since-changed one).
	pendingByKey := make(map[proseKey]string, len(result.Pending))
	for _, pending := range result.Pending {
		pendingByKey[proseKey{IssueID: pending.IssueID, Field: pending.Field}] = pending.Fingerprint()
	}

	resolvedByKey := make(map[proseKey]string, len(resolutions))
	for _, resolution := range resolutions {
		key := proseKey{IssueID: resolution.IssueID, Field: resolution.Field}
		// A resolution for a field that is not pending, or one whose fingerprint
		// does not match the live conflict, means the agent merged against a
		// divergence that no longer matches the current one. Reject the whole set
		// rather than apply a stale merge. [LAW:no-silent-failure]
		liveFingerprint, ok := pendingByKey[key]
		if !ok || resolution.Fingerprint != liveFingerprint {
			return model.Export{}, false
		}
		// A second resolution for the same field is an ambiguous, malformed set:
		// silently keeping the last would finalize one of two conflicting texts the
		// agent supplied. Reject it instead of letting a map overwrite pick. The
		// bijection count below cannot catch this on its own — a duplicate key keeps
		// the map the same size — so the duplicate must fail here. [LAW:no-silent-failure]
		if _, dup := resolvedByKey[key]; dup {
			return model.Export{}, false
		}
		resolvedByKey[key] = resolution.Text
	}
	// Every pending field must be resolved, or the export would still carry a
	// provisional prose value. The two equal-size maps with no rejected key above
	// make this an exact bijection.
	if len(resolvedByKey) != len(pendingByKey) {
		return model.Export{}, false
	}

	export := result.Provisional()
	issues := make([]model.Issue, len(export.Issues))
	copy(issues, export.Issues)
	for i := range issues {
		applyIssueProse(&issues[i], resolvedByKey)
	}
	export.Issues = issues
	return export, true
}

// applyIssueProse writes the resolved text for each of an issue's pending prose
// fields. [LAW:single-enforcer] This is the one mapping from ProseField to the
// concrete export field; ResolveIssue emits these same three fields and nothing
// else ever reaches the agent surface.
func applyIssueProse(issue *model.Issue, resolved map[proseKey]string) {
	for field, set := range map[ProseField]func(string){
		ProseTitle:       func(text string) { issue.Title = text },
		ProseDescription: func(text string) { issue.Description = text },
		ProsePrompt:      func(text string) { issue.Prompt = text },
	} {
		if text, ok := resolved[proseKey{IssueID: issue.ID, Field: field}]; ok {
			set(text)
		}
	}
}

// SortPending orders a pending set deterministically (by issue id, then field) so
// the agent surface renders it the same way every time. [LAW:one-source-of-truth]
// the engine emits in map order; the one place that fixes a display order is here.
func SortPending(pending []ProsePending) []ProsePending {
	out := make([]ProsePending, len(pending))
	copy(out, pending)
	sort.Slice(out, func(i, j int) bool {
		if out[i].IssueID != out[j].IssueID {
			return out[i].IssueID < out[j].IssueID
		}
		return out[i].Field < out[j].Field
	})
	return out
}
