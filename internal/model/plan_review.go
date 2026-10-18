package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// PlanReview is one law review of a ticket's plan: a reviewer's verdict on one
// version of the plan's text, and the laws the verdict cites. It is recorded,
// never inferred — lit holds that the review happened and who gave it, so the
// agent that wrote the plan is not the one vouching for it.
//
// The version is named by the digest of the text it judged, not by the number
// lit history shows it under. That number is a position in the history's
// order, and a sync that merges in an older edit renumbers the versions after
// it; the digest names the same text on every store, after any merge. So a
// verdict on one text says nothing about another, and a plan returned to a
// text that was reviewed is that reviewed text again.
// [LAW:one-source-of-truth] the history holds the text; the review holds only
// the name of it.
type PlanReview struct {
	ID         string      `json:"id"`
	IssueID    string      `json:"issue_id"`
	PlanDigest PlanDigest  `json:"plan_digest"`
	Verdict    PlanVerdict `json:"verdict"`
	// Laws are the laws the verdict cites, in their canonical order: sorted,
	// each once. A fail cites at least one.
	Laws      []Law     `json:"laws"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// Judges reports whether this review judged the plan text plan.
func (r PlanReview) Judges(plan string) bool {
	return r.PlanDigest == DigestPlan(plan)
}

// PlanDigest names one plan text: the hex SHA-256 of it, as stored.
type PlanDigest string

// DigestPlan is the one way a plan text becomes its PlanDigest.
// [LAW:single-enforcer]
func DigestPlan(text string) PlanDigest {
	sum := sha256.Sum256([]byte(text))
	return PlanDigest(hex.EncodeToString(sum[:]))
}

// PlanVerdict is a law review's outcome.
type PlanVerdict string

const (
	PlanPass PlanVerdict = "pass"
	PlanFail PlanVerdict = "fail"
)

// ParsePlanVerdict is the gate a verdict's spelling passes through on its way
// in from a command line.
func ParsePlanVerdict(raw string) (PlanVerdict, error) {
	switch verdict := PlanVerdict(strings.TrimSpace(raw)); verdict {
	case PlanPass, PlanFail:
		return verdict, nil
	}
	return "", ValidationError{Message: fmt.Sprintf("verdict must be %s or %s, got %q", PlanPass, PlanFail, raw)}
}

// Law is one law's token, the name a review cites it by: lower-case words
// joined by hyphens, as a law citation in code spells it. lit does not hold
// the list of laws — they belong to whoever reviews against them — so a token
// is checked for its shape, not looked up.
type Law string

var lawShape = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ParseLaw is the gate a law token passes through on its way in. The shape it
// admits has no comma, which is what lets a review's laws be stored joined by
// one.
func ParseLaw(raw string) (Law, error) {
	token := strings.TrimSpace(raw)
	if !lawShape.MatchString(token) {
		return "", ValidationError{Message: fmt.Sprintf("a law is its token, lower-case words joined by hyphens (one-source-of-truth), got %q", raw)}
	}
	return Law(token), nil
}

// CanonicalLaws is a set of laws in the one order every review carries them
// in: sorted, each once.
func CanonicalLaws(laws []Law) []Law {
	out := append([]Law{}, laws...)
	slices.Sort(out)
	return slices.Compact(out)
}
