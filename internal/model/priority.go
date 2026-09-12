package model

import (
	"errors"
	"strconv"
	"strings"
)

// Priority is the sealed two-level priority domain.
// [LAW:types-are-the-program] Every legal priority is a named constant and
// nothing else is constructible through ParsePriority or ParsePriorityName, so
// downstream code assumes validity instead of re-checking a raw int. Values
// originate from those gates at trust boundaries or from the constants; raw
// conversions are reserved for salvage paths that must conserve values the DB
// CHECK constraint already vouches for.
type Priority int

const (
	PriorityNormal Priority = 0
	PriorityUrgent Priority = 1
)

// priorityVocabulary is the one table the priority domain is spelled in, in
// canonical order, lowest first; its first entry is where out-of-domain ints
// coerce. Every other function here is a read of it in some direction —
// priorityEntry resolves ints onto it, String renders through it,
// ParsePriorityName inverts it, Priorities lists it — so the word a read
// surface prints is by construction a word the write flag accepts.
//
// Those two directions used to be written out separately: the --priority flag
// took an int while every read surface printed a word, so the value an agent
// read off `lit show` could not be pasted into the flag that set it
// (links-cli-bvko). Extending the domain is an edit to this table and nowhere
// else. [LAW:one-source-of-truth]
var priorityVocabulary = []struct {
	value Priority
	name  string
}{
	{PriorityNormal, "normal"},
	{PriorityUrgent, "urgent"},
}

// priorityEntry resolves any raw int onto its vocabulary entry, coercing
// out-of-domain values onto the first entry the way the salvage paths require.
// It is the table's only scan-by-value, so a priority's number and its word can
// never resolve differently. [LAW:one-source-of-truth]
func priorityEntry(v int) (Priority, string) {
	for _, entry := range priorityVocabulary {
		if int(entry.value) == v {
			return entry.value, entry.name
		}
	}
	return priorityVocabulary[0].value, priorityVocabulary[0].name
}

// CanonicalPriority is the single authority on the priority domain: it maps
// any raw int onto the canonical set and is idempotent, so its fixed points ARE
// the legal priorities. ParsePriority (live writes) and the import boundary
// (legacy restores) are both defined in terms of it, so they cannot disagree
// about what a legal priority is. [LAW:one-source-of-truth] [LAW:single-enforcer]
func CanonicalPriority(v int) Priority {
	value, _ := priorityEntry(v)
	return value
}

// Priorities returns the legal priorities in canonical order. Callers that
// render the vocabulary — flag help, usage strings — derive from this rather
// than spelling the set out again. [LAW:one-source-of-truth]
func Priorities() []Priority {
	out := make([]Priority, len(priorityVocabulary))
	for i, entry := range priorityVocabulary {
		out[i] = entry.value
	}
	return out
}

// priorityTokens renders each legal priority with both spellings it answers
// to, so a refusal names every token a caller may type.
func priorityTokens() []string {
	tokens := make([]string, len(priorityVocabulary))
	for i, entry := range priorityVocabulary {
		tokens[i] = entry.name + " (" + strconv.Itoa(int(entry.value)) + ")"
	}
	return tokens
}

var errInvalidPriority = errors.New("priority must be " + oxfordOr(priorityTokens()))

// ParsePriority maps an untrusted priority int (import payload, bulk spec)
// into the sealed set, rejecting exactly the values CanonicalPriority would
// rewrite — i.e. anything that is not already canonical. Live write
// boundaries reject where salvage paths coerce, but both read "what is a
// legal priority" from CanonicalPriority, so the two resolutions stay in
// lockstep. [LAW:single-enforcer]
func ParsePriority(v int) (Priority, error) {
	p := CanonicalPriority(v)
	if int(p) != v {
		return 0, errInvalidPriority
	}
	return p, nil
}

// ParsePriorityName maps an untrusted --priority flag value onto the sealed
// set. It accepts both spellings the read surfaces emit — the word `lit show`
// and the issue rows print, and the decimal `lit export` writes and the
// `lit import` payload carries — because a value a caller reads back has to be
// a value they can paste into the flag that set it. Every other token is
// refused, so `--priority 7` is unrepresentable rather than merely unusual.
//
// [LAW:parse-dont-validate] Returns a Priority, so no write path downstream
// re-checks the range. [LAW:single-enforcer] The only string-to-Priority gate.
func ParsePriorityName(raw string) (Priority, error) {
	token := strings.ToLower(strings.TrimSpace(raw))
	for _, entry := range priorityVocabulary {
		if token == entry.name || token == strconv.Itoa(int(entry.value)) {
			return entry.value, nil
		}
	}
	return 0, errInvalidPriority
}

// String renders the priority's display name — the same word ParsePriorityName
// accepts back. Resolving through the vocabulary keeps it total for the salvage
// paths that hold raw ints the DB CHECK constraint already vouched for.
func (p Priority) String() string {
	_, name := priorityEntry(int(p))
	return name
}
