package model

import (
	"strconv"
	"strings"
	"testing"
)

// priorityProbe is a representative spread across the int domain: below-range,
// the two canonical values, legacy 5-level values (2..4), and far out-of-range.
// The contracts below must hold for every one of them, not just the happy pair.
var priorityProbe = []int{-7, -1, int(PriorityNormal), int(PriorityUrgent), 2, 3, 4, 99}

// The single invariant the sealed type exists to guarantee: the strict gate
// and the salvage normalizer read "what is a legal priority" from the same
// authority, so they cannot disagree about the domain. Concretely,
// ParsePriority accepts a value iff that value is a fixed point of
// CanonicalPriority (i.e. CanonicalPriority would not rewrite it).
func TestParsePriorityAcceptsExactlyCanonicalFixedPoints(t *testing.T) {
	for _, p := range priorityProbe {
		_, err := ParsePriority(p)
		accepted := err == nil
		isFixedPoint := int(CanonicalPriority(p)) == p
		if accepted != isFixedPoint {
			t.Errorf("priority %d: ParsePriority accepts=%v but CanonicalPriority fixed-point=%v; the strict and salvage decisions have drifted", p, accepted, isFixedPoint)
		}
	}
}

// The salvage path coerces rather than rejects; whatever it produces must
// always be a value the strict gate would accept, or a restored row could
// carry a priority the live write path forbids.
func TestCanonicalizedPriorityAlwaysParses(t *testing.T) {
	for _, p := range priorityProbe {
		if _, err := ParsePriority(int(CanonicalPriority(p))); err != nil {
			t.Errorf("priority %d canonicalized to %d which the parse gate rejects: %v", p, CanonicalPriority(p), err)
		}
	}
}

// CanonicalPriority is the domain authority only if it is idempotent — its
// range must equal its set of fixed points, which is what makes the parse
// gate's fixed-point check a faithful test of domain membership.
func TestCanonicalPriorityIsIdempotent(t *testing.T) {
	for _, p := range priorityProbe {
		once := CanonicalPriority(p)
		if twice := CanonicalPriority(int(once)); twice != once {
			t.Errorf("priority %d: CanonicalPriority not idempotent (%d -> %d -> %d)", p, p, once, twice)
		}
	}
}

// Restore tolerance the salvage boundary must preserve: legacy 5-level exports
// (2..4) coerce to normal so a restore is never rejected, while urgent and
// normal survive unchanged.
func TestCanonicalPriorityPreservesRestoreTolerance(t *testing.T) {
	cases := map[int]Priority{
		int(PriorityNormal): PriorityNormal,
		int(PriorityUrgent): PriorityUrgent,
		2:                   PriorityNormal,
		3:                   PriorityNormal,
		4:                   PriorityNormal,
	}
	for in, want := range cases {
		if got := CanonicalPriority(in); got != want {
			t.Errorf("CanonicalPriority(%d) = %d, want %d", in, got, want)
		}
	}
}

// The display names are part of the CLI's observable output contract.
func TestPriorityString(t *testing.T) {
	if got := PriorityNormal.String(); got != "normal" {
		t.Errorf("PriorityNormal.String() = %q, want %q", got, "normal")
	}
	if got := PriorityUrgent.String(); got != "urgent" {
		t.Errorf("PriorityUrgent.String() = %q, want %q", got, "urgent")
	}
}

// links-cli-bvko, stated as the property it is: every word a read surface
// prints is a word the write flag accepts, and it parses back to the priority
// it was printed from. Quantifying over Priorities() rather than naming the two
// words is what makes this survive a third priority — and what kills any change
// that gives String and ParsePriorityName separate spellings of the domain.
func TestPriorityWordRoundTripsThroughTheWriteGate(t *testing.T) {
	for _, want := range Priorities() {
		word := want.String()
		got, err := ParsePriorityName(word)
		if err != nil {
			t.Errorf("priority %d prints %q but the write gate rejects it: %v", int(want), word, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePriorityName(%q) = %d, want %d; the printed word parses back to a different priority", word, int(got), int(want))
		}
	}
}

// The other spelling with a live read surface: `lit export` writes the decimal
// and the `lit import` payload carries it, so the decimal has to round-trip
// too. Dropping it would break every caller that pastes an exported value back.
func TestPriorityDecimalRoundTripsThroughTheWriteGate(t *testing.T) {
	for _, want := range Priorities() {
		decimal := strconv.Itoa(int(want))
		got, err := ParsePriorityName(decimal)
		if err != nil {
			t.Errorf("priority %d exports as %q but the write gate rejects it: %v", int(want), decimal, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePriorityName(%q) = %d, want %d", decimal, int(got), int(want))
		}
	}
}

// The string gate and the int gate are one domain seen through two spellings,
// so they must agree on membership for every probe value — including the legacy
// 5-level values (2..4) that the salvage normalizer coerces but neither strict
// gate may accept. This is what stops `--priority 2` from quietly landing as
// normal the way CanonicalPriority would have it.
func TestParsePriorityNameAgreesWithParsePriorityOnTheDecimalDomain(t *testing.T) {
	for _, v := range priorityProbe {
		_, intErr := ParsePriority(v)
		_, strErr := ParsePriorityName(strconv.Itoa(v))
		if (intErr == nil) != (strErr == nil) {
			t.Errorf("priority %d: ParsePriority accepts=%v but ParsePriorityName accepts=%v; the two spellings disagree about the domain", v, intErr == nil, strErr == nil)
		}
	}
}

// Strictness, pinned fragment by fragment. Every one of these silently became
// normal under a lenient gate, which is exactly how `--priority 7` reached the
// store: the flag's ParseInt was the only thing between an arbitrary int and a
// two-value domain. [LAW:no-silent-failure]
func TestParsePriorityNameRejectsEverythingOutsideTheVocabulary(t *testing.T) {
	rejected := []string{
		"7",             // out of domain, the ticket's own example
		"2",             // legacy 5-level: salvage coerces it, a live write must not
		"-1",            // below domain
		"00",            // not a canonical spelling, though ParseInt would take it
		"high",          // a plausible word that is not in the vocabulary
		"",              // the empty flag value
		" ",             // whitespace only
		"normal,urgent", // priority is single-valued; a set is not a priority
		"0x1",           // another spelling ParseInt-adjacent code might admit
	}
	for _, raw := range rejected {
		if got, err := ParsePriorityName(raw); err == nil {
			t.Errorf("ParsePriorityName(%q) = %d with no error; the gate must refuse every token outside the vocabulary", raw, int(got))
		}
	}
}

// Surrounding whitespace and case are canonicalized, matching ParseIssueType —
// the sibling gate an agent reaches for in the same command.
func TestParsePriorityNameCanonicalizesCaseAndSpace(t *testing.T) {
	for _, want := range Priorities() {
		for _, raw := range []string{
			strings.ToUpper(want.String()),
			"  " + want.String() + "  ",
		} {
			got, err := ParsePriorityName(raw)
			if err != nil {
				t.Errorf("ParsePriorityName(%q): unexpected error %v", raw, err)
				continue
			}
			if got != want {
				t.Errorf("ParsePriorityName(%q) = %d, want %d", raw, int(got), int(want))
			}
		}
	}
}

// The ticket's second half: a parse refusal must name the accepted values,
// because "retry" is not an act that can resolve a deterministic refusal. Both
// spellings of every legal priority have to appear, or the message sends a
// caller looking for a token it declined to mention.
func TestPriorityRefusalNamesEveryAcceptedToken(t *testing.T) {
	_, err := ParsePriorityName("high")
	if err == nil {
		t.Fatal("ParsePriorityName(\"high\") unexpectedly succeeded")
	}
	msg := err.Error()
	for _, p := range Priorities() {
		for _, token := range []string{p.String(), strconv.Itoa(int(p))} {
			if !strings.Contains(msg, token) {
				t.Errorf("refusal %q does not name the accepted token %q", msg, token)
			}
		}
	}
}
