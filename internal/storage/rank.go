package storage

import (
	"fmt"
	"slices"
)

// RankMove reports the pair a relative rank operation actually applied to
// after frame resolution: MovedID was re-ranked relative to AnchorID. When
// the named issue and target are frame-mates these are the inputs unchanged;
// cross-frame, one or both are the containing ancestors that were comparable.
// Callers surface the substitution to the user — moving an issue other than
// the one named must never be silent. [LAW:no-silent-failure]
type RankMove struct {
	MovedID  string
	AnchorID string
}

// Frame names the keyspace an issue's rank is read within: the id of the
// container holding it, or TopLevel for an issue no container holds.
//
// Rank meaning is frame-local — the backlog sorts by (container's rank, own
// rank), so an issue's rank orders it among its frame-mates and against
// nothing else. That makes the frame the scope every neighbor lookup has to be
// taken in: a rank computed against a neighbor from another frame is a
// midpoint between two values no view ever compares, and it lands the issue in
// a keyspace it shares with issues it does not order.
// [LAW:types-are-the-program] A cross-frame rank is an illegal state of the
// keyspace; scoping every lookup by a Frame is what stops one being written.
type Frame string

// TopLevel is the frame holding every issue no container holds. Naming it as a
// frame rather than as the absence of one is what lets a single scoped lookup
// serve children and top-level issues alike, instead of one query shape for
// each. [LAW:dataflow-not-control-flow]
const TopLevel Frame = ""

// RankEnd reports what a rank-to-edge verb did.
//
// Frame is the keyspace the move was scoped to, so a caller can say which
// order actually changed: the top of an epic's children is not the top of the
// queue, and a reader who just watched RankAbove explain its frame
// substitution has no reason to assume this verb behaved differently unless
// told.
//
// Moved is false when the issue already held that edge and nothing was
// written. An unchanged order reported as a success is the outcome nobody
// would question — the command exits 0, prints the issue, and the caller
// believes it promoted something. [LAW:no-silent-failure]
type RankEnd struct {
	Frame Frame
	Moved bool
}

// RankSetResolution pairs each ID named in a rank-set request with the
// representative that was actually ranked after frame resolution. NamedID and
// RankedID are equal for frame-mates; when they differ the caller must surface
// the substitution — ranking a different issue than named is never silent.
// [LAW:no-silent-failure]
type RankSetResolution struct {
	NamedID  string `json:"named_id"`
	RankedID string `json:"ranked_id"`
}

// RankSetResult reports what a rank-set request did: the per-id substitutions,
// and the one frame the whole set was stacked at the top of.
//
// Frame belongs to the call rather than to each resolution because a set has a
// single anchor — every representative lands in the same keyspace, so carrying
// it per-resolution would be N copies of one fact, free to disagree.
// [LAW:one-source-of-truth]
//
// The caller needs it for the same reason RankEnd carries one: the top of an
// epic's children is not the top of the queue, and "ranked 3 issues at top"
// reads as the latter. [LAW:no-silent-failure]
type RankSetResult struct {
	Resolutions []RankSetResolution
	Frame       Frame
}

// RankSetOrder is the order a rank set leaves its frame in: the
// representatives first, in the order named, then every other member of the
// frame in the order it already held.
//
// occupants is the frame's ranked members in their current order, and the
// result names the new occupant of each of those same slots. A rank set is
// therefore a permutation of the frame and nothing else: it mints no key and
// frees none, so it cannot land on a key another issue holds, cannot move
// anything outside the frame, and asked twice leaves exactly what it left
// once. Both engines apply this one answer — the memory engine to its slots,
// the SQL engine to the keys those slots hold — so they cannot disagree about
// what rank set means. [LAW:one-source-of-truth] [LAW:types-are-the-program]
//
// Every representative must be one of the occupants. One that is not — deleted,
// unranked, or outside the frame — would make the result longer than the frame,
// and writing the part that fits would drop a live sibling from the order, so
// the whole request is refused instead. [LAW:no-silent-failure]
func RankSetOrder(f Frame, occupants, reps []string) ([]string, error) {
	named := make(map[string]struct{}, len(reps))
	for _, rep := range reps {
		named[rep] = struct{}{}
	}
	ordered := slices.Clone(reps)
	for _, id := range occupants {
		if _, isRep := named[id]; !isRep {
			ordered = append(ordered, id)
		}
	}
	if len(ordered) != len(occupants) {
		return nil, fmt.Errorf("rank set: %d issues resolved into %s but the frame holds %d ranked — refusing to rewrite a partial order", len(ordered), f, len(occupants))
	}
	return ordered, nil
}
