package storage

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// FrameContainmentError refuses a rank request that names an issue together
// with one of its own ancestors. No comparable frame holds both — the
// contained issue's representative would be (or sit under) the container
// itself — so no frame-coherent rank order between them exists, and the same
// request is refused the same way on every run. Its advice holds whichever
// side was being moved, because a rank set names no moved side to address.
// [LAW:one-source-of-truth] Both engines resolve frames to this one type and
// return it unreworded, so they cannot word or classify the refusal
// differently.
type FrameContainmentError struct {
	ContainerID string
	ContainedID string
}

func (e FrameContainmentError) Error() string {
	return fmt.Sprintf("%s is inside %s; no comparable frame contains both, so neither can be ranked relative to the other — rank each against its own siblings instead", e.ContainedID, e.ContainerID)
}

// RefusesCommand marks FrameContainmentError as a model.Refusal.
func (FrameContainmentError) RefusesCommand() {}

var _ model.Refusal = FrameContainmentError{}

// ValidateRankSetIDs refuses a rank set whose arguments cannot name an order:
// no ids, an empty id, or one id named twice. One id is an order — that issue
// first in its frame — which is what a bulk file with one `rank: top`
// document asks for. The refusals are about the call's own arguments, so they
// carry the "rank set: " prefix.
// [LAW:one-source-of-truth] Both engines run this one check, so they cannot
// word or classify these refusals differently.
func ValidateRankSetIDs(ids []string) error {
	if len(ids) == 0 {
		return model.ValidationError{Message: "rank set: need at least 1 ID to establish order"}
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			return model.ValidationError{Message: "rank set: empty ID in input"}
		}
		if _, dup := seen[id]; dup {
			return model.ValidationError{Message: fmt.Sprintf("rank set: duplicate ID %q in input", id)}
		}
		seen[id] = struct{}{}
	}
	return nil
}

// RankSetResolutions pairs each named id with reps[i], the frame
// representative it resolved to. Two named ids collapsing onto one
// representative is refused: the order asked for places issues from inside one
// epic against outsiders, which no frame-coherent write expresses, and honoring
// the part of it that fits would misrepresent the request.
// [LAW:no-silent-failure] [LAW:one-source-of-truth] Both engines resolve
// through this one pairing.
func RankSetResolutions(ids, reps []string) ([]RankSetResolution, error) {
	resolutions := make([]RankSetResolution, len(ids))
	namedByRep := make(map[string]string, len(ids))
	for i, id := range ids {
		if prior, dup := namedByRep[reps[i]]; dup {
			return nil, model.ValidationError{Message: fmt.Sprintf("rank set: %s and %s both resolve to %s — their relative order is internal to %s and cannot be set against outside issues; run rank set among siblings instead", prior, id, reps[i], reps[i])}
		}
		namedByRep[reps[i]] = id
		resolutions[i] = RankSetResolution{NamedID: id, RankedID: reps[i]}
	}
	return resolutions, nil
}

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
// Every issue has a place, whatever the stored hierarchy holds, because a
// listing is a reader and not the hierarchy's enforcer: the write boundary
// refuses a second parent and a loop, and `lit doctor` reports what restored
// data carries past it. A reader that refused instead would take export,
// backup, sync and `show` down with it — the tools that find and repair the
// fault. [LAW:single-enforcer] A child two parents claim is placed under the
// lower parent id, the frame lookup's rule too, and recorded in Conflicts for
// doctor to name. A chain that loops ends where it repeats.
type RankAncestry struct {
	places    map[string][]rankPlace
	conflicts []ParentConflict
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

// ParentConflict is a child that more than one framing edge claims, with
// every parent claiming it, lowest id first. The write boundary keeps one
// parent per child, so only restored data carries it.
type ParentConflict struct {
	ChildID string
	Parents []string
}

// Finding is the conflict as `lit doctor` reports it.
func (c ParentConflict) Finding() string {
	return fmt.Sprintf("%s has %d parents (%s); it lists under %s, the lowest id, until one remains — run 'lit parent set --child %s --parent <the epic it belongs under>', which replaces every parent it holds with that one", c.ChildID, len(c.Parents), strings.Join(c.Parents, ", "), c.Parents[0], c.ChildID)
}

// NewRankAncestry builds the ancestry from an engine's framing edges.
func NewRankAncestry(links []ParentLink) RankAncestry {
	claims := map[string][]ParentLink{}
	for _, link := range links {
		claims[link.ChildID] = append(claims[link.ChildID], link)
	}
	parentOf := make(map[string]ParentLink, len(claims))
	var conflicts []ParentConflict
	for child, edges := range claims {
		slices.SortFunc(edges, func(a, b ParentLink) int { return strings.Compare(a.ParentID, b.ParentID) })
		parentOf[child] = edges[0]
		if len(edges) > 1 {
			parents := make([]string, len(edges))
			for i, edge := range edges {
				parents[i] = edge.ParentID
			}
			conflicts = append(conflicts, ParentConflict{ChildID: child, Parents: parents})
		}
	}
	slices.SortFunc(conflicts, func(a, b ParentConflict) int { return strings.Compare(a.ChildID, b.ChildID) })
	places := make(map[string][]rankPlace, len(parentOf))
	for child := range parentOf {
		places[child] = placesOf(child, parentOf)
	}
	return RankAncestry{places: places, conflicts: conflicts}
}

// placesOf walks child's chain up to its root, or to the first container it
// would revisit.
func placesOf(child string, parentOf map[string]ParentLink) []rankPlace {
	var places []rankPlace
	visited := map[string]struct{}{child: {}}
	for link, ok := parentOf[child]; ok; link, ok = parentOf[link.ParentID] {
		if _, looped := visited[link.ParentID]; looped {
			break
		}
		visited[link.ParentID] = struct{}{}
		places = append(places, rankPlace{rank: link.ParentRank, id: link.ParentID})
	}
	slices.Reverse(places)
	return places
}

// Conflicts lists every child more than one framing edge claims, by child id.
func (a RankAncestry) Conflicts() []ParentConflict {
	return a.conflicts
}

// Compare orders two issues by tree order; it is what the "rank" sort key
// means in every engine. Two distinct issues never compare equal.
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
// for every group of related issues a view assembles alike.
// [LAW:one-source-of-truth]
func (a RankAncestry) Sort(issues []model.Issue) {
	slices.SortFunc(issues, a.Compare)
}

// SortSiblings puts children of one parent in order. Siblings share a frame, so
// they compare on their own keys and no ancestor's key is read: the empty
// ancestry is tree order among them.
func SortSiblings(issues []model.Issue) {
	RankAncestry{}.Sort(issues)
}

// FrameRepresentatives maps each ancestor chain (self first, root last) onto
// its representative in the chains' comparable frame. Rank meaning is
// frame-local: an issue's rank is only ever compared against its frame-mates
// (siblings under the same container, or fellow top-level items), so issues
// from different frames resolve to their representatives directly under the
// lowest common ancestor of all chains — and to their roots when the
// ancestries share none, the top level being the comparable frame. Nothing
// inside any epic is ever reordered by a cross-frame request. A chain whose
// issue is a container of another chain's is refused with
// [FrameContainmentError]: tree order lists a container before everything
// inside it, so no frame holds both.
//
// The frame the representatives live in comes back with them: it is the lowest
// common ancestor the walk already found, and it is the scope every neighbor
// lookup seeded by these representatives must be taken in. Returning it here
// rather than re-deriving it at each callsite keeps the representatives and
// their keyspace from ever disagreeing. [LAW:one-source-of-truth]
//
// [LAW:types-are-the-program] Cross-frame midpoints are an illegal state of
// the rank keyspace; this resolution makes every write frame-coherent.
// [LAW:single-enforcer] The one resolution core behind every rank verb in both
// engines, and behind [RankAncestry.Representatives].
func FrameRepresentatives(chains [][]string) ([]string, Frame, error) {
	memberships := make([]map[string]int, len(chains))
	for i, chain := range chains {
		m := make(map[string]int, len(chain))
		for idx, id := range chain {
			m[id] = idx
		}
		memberships[i] = m
	}
	for i, chain := range chains {
		for j, m := range memberships {
			if i == j {
				continue
			}
			if idx, ok := m[chain[0]]; ok && idx > 0 {
				return nil, TopLevel, FrameContainmentError{ContainerID: chain[0], ContainedID: chains[j][0]}
			}
		}
	}
	// Common ancestors form a shared suffix of every chain, so the first
	// proper ancestor of chains[0] present in all others is the lowest common
	// ancestor. The issue itself is never it: in two or more chains that
	// would be the containment refused above, and a lone chain's frame is its
	// issue's parent.
	lcaID := ""
	for _, id := range chains[0][1:] {
		inAll := true
		for _, m := range memberships[1:] {
			if _, ok := m[id]; !ok {
				inAll = false
				break
			}
		}
		if inAll {
			lcaID = id
			break
		}
	}
	reps := make([]string, len(chains))
	for i, chain := range chains {
		if lcaID == "" {
			reps[i] = chain[len(chain)-1]
			continue
		}
		reps[i] = chain[memberships[i][lcaID]-1]
	}
	// No common ancestor means the representatives are roots, and the frame
	// holding every root is the top level — which Frame spells as the same
	// empty string lcaID already carries.
	return reps, Frame(lcaID), nil
}

// Representatives resolves x and y to the frame-mates tree order decides them
// by: [FrameRepresentatives] over their places in the ancestry. ok is false
// when x and y are one issue or one contains the other, which tree order
// orders and no key can.
func (a RankAncestry) Representatives(x, y string) (xRep, yRep string, ok bool) {
	if x == y {
		return "", "", false
	}
	reps, _, err := FrameRepresentatives([][]string{a.chain(x), a.chain(y)})
	// Containment is the one refusal FrameRepresentatives makes.
	if err != nil {
		return "", "", false
	}
	return reps[0], reps[1], true
}

// chain is id followed by its containers, innermost first.
func (a RankAncestry) chain(id string) []string {
	places := a.places[id]
	chain := make([]string, 0, len(places)+1)
	chain = append(chain, id)
	for i := len(places) - 1; i >= 0; i-- {
		chain = append(chain, places[i].id)
	}
	return chain
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
