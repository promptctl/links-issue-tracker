package storage

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

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

// RankAncestry holds, for every issue a container frames, the place of each
// of its containers, outermost first. An issue absent from it sits at the top
// level.
//
// It exists because a key is only comparable within its frame, and a listing
// still has to put every issue in one sequence. The sequence is tree order:
// compare the containers' places from the outermost down, then the issues'
// own, and when one issue's places run out first, it comes first. A
// container's places are a prefix of every descendant's, so it lists before
// them and its subtree lists together, whatever keys other frames hold in
// between. That leaves no cross-frame arrangement of keys that a listing can
// show, so no rank verb has to keep one. [LAW:types-are-the-program]
//
// An issue with two parents, or whose parent chain loops, has no one place,
// and neither does anything beneath it. Those issues are recorded with the
// reason rather than failing the whole ancestry, so the refusal reaches only
// the views that hold one: an unrelated listing or `lit show` still answers,
// and the repair stays reachable. [LAW:no-silent-failure]
type RankAncestry struct {
	places   map[string][]rankPlace
	unplaced map[string]error
}

// rankPlace is one step of an issue's path through tree order: a key, and the
// id that breaks a tie on it. The tie is broken at every step rather than once
// at the end, because two frame-mates can share a key (restored data, or the
// empty key every unranked issue holds), and a tie left open at a container's
// step hands the decision to the next step — which files an outsider between
// the container and its children, and interleaves two tied containers'
// subtrees.
type rankPlace struct {
	rank string
	id   string
}

func (p rankPlace) compare(q rankPlace) int {
	return cmp.Or(strings.Compare(p.rank, q.rank), strings.Compare(p.id, q.id))
}

// TwoParentsError reports a child that two framing edges claim. The write
// boundary keeps one parent per child, so only restored data carries it.
type TwoParentsError struct {
	ChildID string
	Parents [2]string
}

func (e TwoParentsError) Error() string {
	return fmt.Sprintf("%s has two parents, %s and %s, so it has no one place in rank order; run 'lit parent clear %s', then set the parent it belongs under", e.ChildID, e.Parents[0], e.Parents[1], e.ChildID)
}

// NewRankAncestry builds the ancestry from an engine's framing edges.
func NewRankAncestry(links []ParentLink) RankAncestry {
	parentOf := make(map[string]ParentLink, len(links))
	twoParents := map[string]TwoParentsError{}
	for _, link := range links {
		if prior, dup := parentOf[link.ChildID]; dup {
			twoParents[link.ChildID] = TwoParentsError{ChildID: link.ChildID, Parents: [2]string{prior.ParentID, link.ParentID}}
			continue
		}
		parentOf[link.ChildID] = link
	}
	ancestry := RankAncestry{places: make(map[string][]rankPlace, len(parentOf)), unplaced: map[string]error{}}
	for child := range parentOf {
		if err := ancestry.walk(child, parentOf, twoParents); err != nil {
			ancestry.unplaced[child] = err
		}
	}
	return ancestry
}

// walk records child's containers' places, or reports why it has none: a
// chain that passes through a child two parents claim, or that loops.
func (a RankAncestry) walk(child string, parentOf map[string]ParentLink, twoParents map[string]TwoParentsError) error {
	var places []rankPlace
	visited := map[string]struct{}{child: {}}
	for id := child; ; {
		if conflict, ok := twoParents[id]; ok {
			return conflict
		}
		link, ok := parentOf[id]
		if !ok {
			break
		}
		if _, looped := visited[link.ParentID]; looped {
			return fmt.Errorf("the parent chain of %s loops back to %s, so it has no place in rank order; 'lit doctor' names the cycle, and 'lit parent clear' on one member breaks it", child, link.ParentID)
		}
		visited[link.ParentID] = struct{}{}
		places = append(places, rankPlace{rank: link.ParentRank, id: link.ParentID})
		id = link.ParentID
	}
	slices.Reverse(places)
	a.places[child] = places
	return nil
}

// Place reports whether every issue given has a place in tree order, naming
// the first by id that does not. A view calls it on exactly the issues it
// will order, so the refusal is as wide as the fault and no wider.
func (a RankAncestry) Place(issues []model.Issue) error {
	var first *model.Issue
	for i := range issues {
		if _, bad := a.unplaced[issues[i].ID]; bad && (first == nil || issues[i].ID < first.ID) {
			first = &issues[i]
		}
	}
	if first == nil {
		return nil
	}
	return a.unplaced[first.ID]
}

// Refusal is the first refusal by id across every framing edge the ancestry
// was built from — what `lit doctor` reports, whichever views hold the issue.
func (a RankAncestry) Refusal() error {
	ids := slices.Sorted(maps.Keys(a.unplaced))
	if len(ids) == 0 {
		return nil
	}
	return a.unplaced[ids[0]]
}

// Compare orders two issues by tree order; it is what the "rank" sort key
// means in every engine. Two distinct issues never compare equal. It reads
// only issues [RankAncestry.Place] has passed.
func (a RankAncestry) Compare(x, y model.Issue) int {
	xs, ys := a.places[x.ID], a.places[y.ID]
	for i := 0; ; i++ {
		xp, xok := placeAt(xs, x, i)
		yp, yok := placeAt(ys, y, i)
		if !xok || !yok {
			// The path that ran out first is the container's.
			return cmp.Compare(presence(xok), presence(yok))
		}
		if c := xp.compare(yp); c != 0 {
			return c
		}
	}
}

// Sort puts issues in tree order: the one ordering by rank, for a listing and
// for every group of related issues a view assembles alike. It refuses, as
// [RankAncestry.Place] does, a group holding an issue with no place.
// [LAW:one-source-of-truth]
func (a RankAncestry) Sort(issues []model.Issue) error {
	if err := a.Place(issues); err != nil {
		return err
	}
	slices.SortFunc(issues, a.Compare)
	return nil
}

// placeAt is step i of an issue's path: its containers' places, then its own.
func placeAt(containers []rankPlace, issue model.Issue, i int) (rankPlace, bool) {
	switch {
	case i < len(containers):
		return containers[i], true
	case i == len(containers):
		return rankPlace{rank: issue.Rank, id: issue.ID}, true
	default:
		return rankPlace{}, false
	}
}

func presence(ok bool) int {
	if ok {
		return 1
	}
	return 0
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
