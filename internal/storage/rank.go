package storage

import (
	"fmt"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/model"
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
// Rank meaning is frame-local — listings sort in tree order (see
// [RankAncestry]), so an issue's rank orders it among its frame-mates and
// against nothing else. That makes the frame the scope every neighbor lookup has to be
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

// ParentLink is one parent-child edge whose parent frames its child — the
// parent is not deleted — together with the key that parent holds in its own
// frame. It is what an engine hands [NewRankAncestry]; which edges qualify is
// the same rule each engine's frame lookup applies.
type ParentLink struct {
	ChildID    string
	ParentID   string
	ParentRank string
}

// RankAncestry holds, for every issue a container frames, the keys of its
// containers outermost first. An issue absent from it sits at the top level.
//
// It exists because a key is only comparable within its frame, and a listing
// still has to put every issue in one sequence. The sequence is tree order:
// compare the containers' keys from the outermost down, then the issues' own,
// and when one issue's keys run out first, it comes first. A container's keys
// are a prefix of every descendant's, so it lists before them and its subtree
// lists together, whatever keys other frames hold in between. That leaves no
// cross-frame arrangement of keys that a listing can show, so no rank verb has
// to keep one. [LAW:types-are-the-program]
type RankAncestry map[string][]string

// NewRankAncestry builds the ancestry from an engine's framing edges.
//
// An issue with two parents, or a parent chain that loops, has no one place in
// tree order, so both are refused by name rather than listed in an order that
// guessed. [LAW:no-silent-failure]
func NewRankAncestry(links []ParentLink) (RankAncestry, error) {
	parentOf := make(map[string]ParentLink, len(links))
	for _, link := range links {
		if prior, dup := parentOf[link.ChildID]; dup {
			return nil, fmt.Errorf("%s has two parents, %s and %s, so it has no one place in rank order; run 'lit parent clear %s', then set the parent it belongs under", link.ChildID, prior.ParentID, link.ParentID, link.ChildID)
		}
		parentOf[link.ChildID] = link
	}
	ancestry := make(RankAncestry, len(parentOf))
	for child := range parentOf {
		var keys []string
		visited := map[string]struct{}{child: {}}
		for link, ok := parentOf[child]; ok; link, ok = parentOf[link.ParentID] {
			if _, looped := visited[link.ParentID]; looped {
				return nil, fmt.Errorf("the parent chain of %s loops back to %s, so it has no place in rank order; 'lit doctor' names the cycle, and 'lit parent clear' on one member breaks it", child, link.ParentID)
			}
			visited[link.ParentID] = struct{}{}
			keys = append(keys, link.ParentRank)
		}
		slices.Reverse(keys)
		ancestry[child] = keys
	}
	return ancestry, nil
}

// Compare orders two issues by tree order; it is what the "rank" sort key
// means in every engine.
func (a RankAncestry) Compare(x, y model.Issue) int {
	return slices.Compare(a.path(x), a.path(y))
}

func (a RankAncestry) path(issue model.Issue) []string {
	return append(slices.Clip(a[issue.ID]), issue.Rank)
}

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
