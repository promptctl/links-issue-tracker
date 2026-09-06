package merge

import (
	"sort"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Collision is two INDEPENDENTLY CREATED tickets that minted the same id on two
// disconnected stores. It is not a diverged ticket. There is no shared intent to
// combine, so a field merge over the pair would not resolve a conflict — it would
// invent a row nobody wrote, with one job's title over another job's description.
// Both rows travel whole so the surface can name the two pieces of work that
// collided instead of reporting a count. Ours is local and Theirs remote — the only
// provenance a reconcile has, since every export it reads carries its own workspace id.
type Collision struct {
	IssueID string      `json:"issue_id"`
	Ours    model.Issue `json:"ours"`
	Theirs  model.Issue `json:"theirs"`
}

// SameEntity is a three-way input PROVEN to describe one ticket: ours and theirs
// are two versions of a single row, so combining their fields merges one author's
// edits with another's rather than fusing two unrelated jobs. Its fields are
// unexported, so outside this package Classify is the only way to obtain one and
// "field-merge a collision" cannot be expressed.
// [LAW:parse-dont-validate] The type IS the proof the check ran, so ResolveIssue
// asks no questions about its input and no caller can re-ask them.
type SameEntity struct {
	base             *model.Issue
	ours, theirs     model.Issue
	oursWS, theirsWS string
}

// Classify is the one checkpoint between "two rows share an id" and "two versions
// of one ticket". A non-nil *Collision is the typed failure arm: the rows are two
// tickets, there is nothing to merge, and the caller must report rather than
// resolve.
//
// It reads the strongest evidence available, in order.
//
// ANCESTRY, when there is any. A merge-base row for this id means both sides
// descend from one creation, so they are one ticket however far their fields have
// drifted — proof, not inference, and it is why base != nil short-circuits.
//
// THE BIRTH CERTIFICATE, when there is not. created_at is fixed when a ticket is
// minted, never edited afterwards, and rides every replication path unchanged, so
// two base-less rows carrying one id are the same ticket exactly when they were
// born at the same instant. Two `lit new` calls on two machines are two instants;
// a replica of one ticket is one instant twice.
//
// Base-absence alone CANNOT decide it, which is the whole trap: the
// unrelated-history combine (Store.combineFromAnchors) feeds this engine an empty
// base by construction, so "no merge-base" is uniform on that path and would
// condemn every legitimately shared row. The second reading is what makes a nil
// base mean one thing again. [LAW:parse-dont-validate] the ambiguity the old
// signature carried is resolved here, once, rather than downstream.
//
// The rule errs toward the loud side by construction: two instants that are
// genuinely one ticket would be reported to an operator (recoverable), where one
// instant read as two would fuse silently (the defect this replaces).
// [LAW:no-silent-failure]
func Classify(base *model.Issue, ours, theirs model.Issue, oursWS, theirsWS string) (SameEntity, *Collision) {
	// Equal, not ==: created_at round-trips through RFC3339Nano, so two encodings
	// of one instant may differ in offset while naming the same moment.
	if base == nil && !ours.CreatedAt.Equal(theirs.CreatedAt) {
		return SameEntity{}, &Collision{IssueID: ours.ID, Ours: ours, Theirs: theirs}
	}
	return SameEntity{base: base, ours: ours, theirs: theirs, oursWS: oursWS, theirsWS: theirsWS}, nil
}

// SortCollisions orders collisions by issue id so every surface — report, trace,
// test — reads them in one stable order. [LAW:one-source-of-truth] the ordering
// lives here, not in each renderer.
func SortCollisions(collisions []Collision) []Collision {
	out := make([]Collision, len(collisions))
	copy(out, collisions)
	sort.Slice(out, func(i, j int) bool { return out[i].IssueID < out[j].IssueID })
	return out
}
