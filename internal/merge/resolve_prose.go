package merge

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Fingerprint names the EXACT three-way conflict one pending field represents — a
// digest of the issue, the field and base/ours/theirs. The agent merges against a
// specific (base, ours, theirs); if any of those changed before it finalizes (the
// remote advanced, or a local edit landed), the fingerprint changes, so a merged
// text produced for the old conflict matches nothing and is not committed.
// [LAW:types-are-the-program] "this text was merged against THIS conflict" is a
// checkable value rather than an assumption.
//
// It is the whole address a resolution carries, which is why it covers the issue
// id. An id is remote-authored and unconstrained, so a resolve command that
// repeated it would carry remote text into a shell line the agent runs; the
// fingerprint is lowercase hex lit computed, whatever the id holds.
type Fingerprint string

// fingerprintBytes is how much of the digest a Fingerprint keeps. A pending set is
// a handful of fields, and two of them sharing a truncation fail the bijection in
// ApplyProseResolutions rather than receiving one text.
const fingerprintBytes = 6

func (p ProsePending) Fingerprint() Fingerprint {
	sum := sha256.Sum256([]byte(p.IssueID + "\x00" + string(p.Field) + "\x00" + p.Base + "\x00" + p.Ours + "\x00" + p.Theirs))
	return Fingerprint(hex.EncodeToString(sum[:fingerprintBytes]))
}

// ParseFingerprint admits exactly the rendering Fingerprint produces, so a value in
// any other shape — the retired ID:FIELD:FINGERPRINT address among them — is
// refused where it enters instead of reaching the store as a fingerprint that
// quietly matches nothing. [LAW:parse-dont-validate]
func ParseFingerprint(text string) (Fingerprint, bool) {
	raw, err := hex.DecodeString(text)
	if err != nil || len(raw) != fingerprintBytes || hex.EncodeToString(raw) != text {
		return "", false
	}
	return Fingerprint(text), true
}

// ProseResolution is the calling agent's semantic merge of one prose field that
// diverged on both sides: the single coherent Text that preserves BOTH the Ours
// and Theirs intent. It is the enactment of the one judgment step the engine
// deliberately refuses to make. [LAW:decomposition] The agent owns the decision
// (the Text); this package owns only where that text lands in the export.
//
// Fingerprint names the conflict the agent merged against, copied from the pending
// field's guidance. It is looked up among the LIVE conflicts before the text is
// spliced, so a merge produced for a since-changed conflict finds nothing and is
// rejected rather than silently applied. [LAW:no-silent-failure]
type ProseResolution struct {
	Fingerprint Fingerprint
	Text        string
}

// proseKey identifies one prose field of one issue — the place a resolved text
// lands, reached through the live conflict a resolution's fingerprint names.
// [LAW:types-are-the-program] Making the place a comparable key lets the bijection
// check be a set comparison, not field-by-field prose.
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
// changed underneath it. A merge holding an id collision has no export to splice
// into at all, which Provisional reports rather than this function re-deriving.
//
// It is pure: the live pending set comes from the MergeResult, the merged text
// from the agent — no IO, no clock. [LAW:effects-at-boundaries]
func ApplyProseResolutions(result MergeResult, resolutions []ProseResolution) (model.Export, bool) {
	// Each pending field is addressed by the fingerprint of its LIVE conflict, so
	// one lookup settles both which field a resolution is for and that it was
	// merged against THIS conflict, not a since-changed one.
	pendingByFingerprint := make(map[Fingerprint]proseKey, len(result.Pending))
	for _, pending := range result.Pending {
		pendingByFingerprint[pending.Fingerprint()] = proseKey{IssueID: pending.IssueID, Field: pending.Field}
	}

	resolvedByKey := make(map[proseKey]string, len(resolutions))
	for _, resolution := range resolutions {
		// A fingerprint that names no live conflict means the agent merged against
		// a divergence that no longer matches the current one. Reject the whole set
		// rather than apply a stale merge. [LAW:no-silent-failure]
		key, ok := pendingByFingerprint[resolution.Fingerprint]
		if !ok {
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
	// provisional prose value. Counted against the pending set itself rather than
	// the fingerprint index: two live conflicts that truncate to one fingerprint
	// share an index entry, and must fail here instead of passing as one field.
	if len(resolvedByKey) != len(result.Pending) {
		return model.Export{}, false
	}

	export, ok := result.Provisional()
	if !ok {
		return model.Export{}, false
	}
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
