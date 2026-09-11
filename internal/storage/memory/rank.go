package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The rank verbs move issues within lit's one total order, and they do it only
// through relative intents. Here the order is a slice, so an intent is exactly
// what it says: "above Y" removes the issue and puts it back immediately
// before Y. There is no fractional key to run out of precision, no midpoint to
// compute, and no inversion to repair — the outcomes the contract names are
// the whole implementation. That is the point of a second engine: the intent
// vocabulary was the contract, and fractional indexing was one way to serve
// it.

func (e *Engine) RankAbove(ctx context.Context, issueID, targetID string) (storage.RankMove, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rankRelative(issueID, targetID, above)
}

func (e *Engine) RankBelow(ctx context.Context, issueID, targetID string) (storage.RankMove, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rankRelative(issueID, targetID, below)
}

// side names which end of the anchor an intent lands on. It is a value the one
// relative-rank path takes, not two paths that could drift about what frame
// resolution means. [LAW:dataflow-not-control-flow]
type side int

const (
	above side = 0
	below side = 1
)

func (e *Engine) rankRelative(issueID, targetID string, at side) (storage.RankMove, error) {
	move, err := e.resolveRankPair(issueID, targetID)
	if err != nil {
		return storage.RankMove{}, err
	}
	e.detach(move.MovedID)
	anchor := slices.Index(e.order, move.AnchorID)
	e.insertAt(anchor+int(at), move.MovedID)
	return move, nil
}

// RankToTop moves an issue to the top of its own frame.
func (e *Engine) RankToTop(ctx context.Context, issueID string) (storage.RankEnd, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rankToEdge(issueID, storage.RankTop)
}

// RankToBottom moves an issue to the bottom of its own frame.
func (e *Engine) RankToBottom(ctx context.Context, issueID string) (storage.RankEnd, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rankToEdge(issueID, storage.RankBottom)
}

// rankToEdge moves an issue to one end of its own frame.
//
// The ends are a frame's, not the slice's. One sequence holds every frame here,
// so "the top" is a position found among the issue's frame-mates — immediately
// before the first of them — and an epic's child sent there leads its siblings
// while every issue outside its frame stays exactly where it was. Index zero
// would instead read as leading the whole backlog, which is a position the
// child's rank never claims.
func (e *Engine) rankToEdge(issueID string, placement storage.RankPlacement) (storage.RankEnd, error) {
	if err := e.mustRankable(issueID); err != nil {
		return storage.RankEnd{}, err
	}
	edge, err := orderEdgeFor(placement)
	if err != nil {
		return storage.RankEnd{}, err
	}
	result := storage.RankEnd{Frame: e.frameOf(issueID)}
	mates := e.frameMateIndexes(result.Frame, issueID)
	// An issue alone in its frame has no end to move to: there is nothing its
	// position is read against. Said out loud, never implied by a success.
	// [LAW:no-silent-failure]
	if len(mates) == 0 {
		return result, nil
	}
	result.Moved = !edge.past(mates, slices.Index(e.order, issueID))
	if !result.Moved {
		return result, nil
	}
	e.detach(issueID)
	// The mates' indexes shift when the issue leaves the order, so the landing
	// slot is read off the order it will actually be inserted into. Detaching
	// removes only the issue, which frameMateIndexes was already excluding, so
	// this is the same set of mates at new positions — never an empty one.
	e.insertAt(edge.positionIn(e.frameMateIndexes(result.Frame, issueID)), issueID)
	return result, nil
}

// orderEdge is one end of a frame expressed as questions about positions in
// the single order: the slot an issue takes to land at that end, and the test
// for an issue already sitting past it. The end a caller asked for crosses as
// this value, so both ends share one placement path.
// [LAW:dataflow-not-control-flow]
type orderEdge struct {
	name string
	// slot answers for a population that has members; positionIn wraps it with
	// the answer for one that has none.
	slot func(mateIndexes []int) int
	past func(mateIndexes []int, issueIndex int) bool
}

// positionIn is where an id lands at this end of a population.
//
// A population with no members has exactly one position, and it is zero.
// Absorbing that here is what lets place assign unconditionally, the way
// rankBeyond absorbs the empty frame for the SQL engine. The alternative —
// answering it in the caller, before the dispatch — is what this had before,
// and it meant the first issue created in a workspace never reached the
// dispatch at all, so it accepted any placement whatsoever while the second
// issue with the same placement was correctly refused.
// [LAW:dataflow-not-control-flow]
func (e orderEdge) positionIn(mateIndexes []int) int {
	if len(mateIndexes) == 0 {
		return 0
	}
	return e.slot(mateIndexes)
}

// orderEdgeFor is the single dispatch point on RankPlacement for positions:
// creation's placement and both edge verbs resolve their end here.
// [LAW:single-enforcer]
//
// It dispatches on the placement alone. The population is a separate question,
// asked of the resolved edge, so an unrecognized placement is refused whatever
// population it was asked about — including none, where there is no end to
// speak of but the placement is just as wrong. [LAW:parse-dont-validate]
func orderEdgeFor(p storage.RankPlacement) (orderEdge, error) {
	switch p {
	case storage.RankTop:
		return orderEdge{
			name: "top",
			slot: func(mateIndexes []int) int { return mateIndexes[0] },
			past: func(mateIndexes []int, issueIndex int) bool { return issueIndex < mateIndexes[0] },
		}, nil
	case storage.RankBottom:
		return orderEdge{
			name: "bottom",
			slot: func(mateIndexes []int) int { return mateIndexes[len(mateIndexes)-1] + 1 },
			past: func(mateIndexes []int, issueIndex int) bool {
				return issueIndex > mateIndexes[len(mateIndexes)-1]
			},
		}, nil
	default:
		return orderEdge{}, fmt.Errorf("unknown rank placement: %d", p)
	}
}

// mustRankable is the one gate every rank verb passes a named issue through:
// it must exist, and it must not be in the trash.
//
// Rank is a position in an order that only lists live issues, so a deleted one
// has no position to hold and nothing to hold it against. Letting it through
// used to mean one of two silent wrongs depending on the verb — a key written
// onto a row no view shows, or, once RankSet began rewriting its frame's slots
// in place, a live sibling dropped out of the order to make room for it.
// Refusing here is what makes both unrepresentable rather than handled.
// [LAW:single-enforcer] [LAW:parse-dont-validate]
func (e *Engine) mustRankable(id string) error {
	if _, err := e.mustRecord(id); err != nil {
		return err
	}
	if !e.live(id) {
		return fmt.Errorf("cannot rank deleted issue %s; restore it first", id)
	}
	return nil
}

// live reports whether an id is still present and undeleted. Deleting an issue
// only flips its retention — it keeps its slot in e.order forever — so every
// lookup here that mirrors a SQL query carrying `deleted_at IS NULL` has to ask
// this rather than trust the slice. [LAW:one-source-of-truth]
func (e *Engine) live(id string) bool {
	rec, ok := e.issues[id]
	if !ok {
		return false
	}
	_, gone := rec.retention.(model.Deleted)
	return !gone
}

// frameMateIndexes lists where an issue's live frame-mates sit in the order,
// ascending, leaving the issue itself out. Liveness is part of the question:
// the SQL engine's matching lookup carries `deleted_at IS NULL`, and an engine
// that anchored against a deleted mate would report moving past a row nobody
// can see. [LAW:one-source-of-truth]
func (e *Engine) frameMateIndexes(f storage.Frame, exclude string) []int {
	var indexes []int
	for index, id := range e.order {
		if id == exclude || !e.live(id) || e.frameOf(id) != f {
			continue
		}
		indexes = append(indexes, index)
	}
	return indexes
}

// frameOf names the frame an issue's position is read within: its container,
// or the top level. Built on parentOf, the one place here that knows what
// contains what. [LAW:one-source-of-truth]
func (e *Engine) frameOf(id string) storage.Frame {
	parent, ok := e.parentOf(id)
	if !ok {
		return storage.TopLevel
	}
	return storage.Frame(parent)
}

// RankSet imposes a total order on the named issues at once, stacking them at
// the top of the representatives' own frame in the order named, and reports
// which representative each name resolved to. The anchor is that frame's top,
// never the whole order's.
func (e *Engine) RankSet(ctx context.Context, ids []string) ([]storage.RankSetResolution, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if len(ids) < 2 {
		return nil, errors.New("rank set: need at least 2 IDs to establish order")
	}
	seen := map[string]struct{}{}
	for _, id := range ids {
		if id == "" {
			return nil, errors.New("rank set: empty ID in input")
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("rank set: duplicate ID %q in input", id)
		}
		seen[id] = struct{}{}
		// Unwrapped, unlike the two input-shape errors above: this refusal has a
		// counterpart on the SQL engine, which returns it bare, and the two must
		// read identically for the same request. The "rank set: " prefix stays on
		// the errors that are about this call's arguments and have no twin.
		// [LAW:one-source-of-truth]
		if err := e.mustRankable(id); err != nil {
			return nil, err
		}
	}
	chains := make([][]string, len(ids))
	for i, id := range ids {
		chain, err := e.ancestorChain(id)
		if err != nil {
			return nil, err
		}
		chains[i] = chain
	}
	reps, err := frameRepresentatives(chains)
	if err != nil {
		return nil, fmt.Errorf("rank set: %w", err)
	}
	// Two named ids collapsing onto one representative is refused: the order
	// asked for places issues from inside one epic against outsiders, which no
	// frame-coherent write expresses, and honoring the part of it that fits
	// would misrepresent the request. [LAW:no-silent-failure]
	namedByRep := map[string]string{}
	resolutions := make([]storage.RankSetResolution, len(ids))
	for i, id := range ids {
		if prior, dup := namedByRep[reps[i]]; dup {
			return nil, fmt.Errorf("rank set: %s and %s both resolve to %s — their relative order is internal to %s and cannot be set against outside issues; run rank set among siblings instead", prior, id, reps[i], reps[i])
		}
		namedByRep[reps[i]] = id
		resolutions[i] = storage.RankSetResolution{NamedID: id, RankedID: reps[i]}
	}
	// The stack lands at the head of the representatives' own frame. Every
	// representative is a frame-mate by construction, so that frame is the only
	// keyspace this order is ever read in; prepending to e.order — what this did
	// before — shoved an epic's children ahead of every top-level issue and every
	// other epic's, the cross-frame bleed the SQL engine stopped committing.
	// [LAW:one-source-of-truth] the two engines are one behavior.
	//
	// The frame's slots are rewritten in place rather than detached and
	// reinserted: the positions the frame already occupies stay exactly where
	// they are and only their occupants are permuted, so nothing outside the
	// frame moves and the frame keeps its place even when the representatives
	// are all of it.
	f := e.frameOf(reps[0])
	slots := e.frameMateIndexes(f, "")
	named := make(map[string]struct{}, len(reps))
	for _, rep := range reps {
		named[rep] = struct{}{}
	}
	ordered := slices.Clone(reps)
	for _, slot := range slots {
		if _, isRep := named[e.order[slot]]; !isRep {
			ordered = append(ordered, e.order[slot])
		}
	}
	// The rewrite permutes the frame's occupants among the frame's own slots, so
	// the two sides have to name the same set. They do once every representative
	// is a live frame-mate, which mustRankable is what guarantees: slots counts
	// only live members, so a deleted representative reaching here would make
	// ordered the longer of the two and the loop below would write a prefix of it
	// — dropping whichever live sibling sat in the slots that ran out, and leaving
	// it in no position at all. A permutation that cannot account for every slot
	// is a resolution bug, and it stops here rather than committing the half of
	// itself that fits. [LAW:no-silent-failure]
	if len(ordered) != len(slots) {
		return nil, fmt.Errorf("rank set: %d issues resolved into %s but the frame holds %d ranked — refusing to rewrite a partial order", len(ordered), f, len(slots))
	}
	for i, slot := range slots {
		e.order[slot] = ordered[i]
	}
	return resolutions, nil
}

// detach lifts an id out of the order, leaving the rest of the sequence
// intact. Every rank verb is a detach followed by a placement, which is what
// makes "the order after the move" a fact about the slice rather than a
// consequence of arithmetic on keys.
func (e *Engine) detach(id string) {
	e.order = slices.DeleteFunc(e.order, func(existing string) bool { return existing == id })
}

// insertAt puts an id at a position. It clamps nothing: every caller derives
// the index from a population it has already read, so the position is in range
// by construction, and a clamp would turn a resolution bug into a silent
// placement at the top of the backlog. [LAW:no-defensive-null-guards]
func (e *Engine) insertAt(index int, id string) {
	e.order = slices.Insert(e.order, index, id)
}

// --- frames ---------------------------------------------------------------

// resolveRankPair validates a relative rank request and resolves it to the
// frame-comparable pair the request is actually about.
// [LAW:single-enforcer] Both relative verbs route through this one resolution,
// so cross-frame semantics cannot drift between above and below.
func (e *Engine) resolveRankPair(issueID, targetID string) (storage.RankMove, error) {
	if issueID == targetID {
		return storage.RankMove{}, errors.New("cannot rank an issue relative to itself")
	}
	if err := e.mustRankable(targetID); err != nil {
		return storage.RankMove{}, err
	}
	if err := e.mustRankable(issueID); err != nil {
		return storage.RankMove{}, err
	}
	issueChain, err := e.ancestorChain(issueID)
	if err != nil {
		return storage.RankMove{}, err
	}
	targetChain, err := e.ancestorChain(targetID)
	if err != nil {
		return storage.RankMove{}, err
	}
	reps, err := frameRepresentatives([][]string{issueChain, targetChain})
	var containment *frameContainmentError
	if errors.As(err, &containment) {
		if containment.containerID == issueID {
			return storage.RankMove{}, fmt.Errorf("cannot rank %s relative to %s: %s contains it; rank it against a sibling instead", issueID, targetID, issueID)
		}
		return storage.RankMove{}, fmt.Errorf("cannot rank %s relative to %s: %s is inside %s; rank it against a sibling instead", issueID, targetID, issueID, targetID)
	}
	if err != nil {
		return storage.RankMove{}, err
	}
	return storage.RankMove{MovedID: reps[0], AnchorID: reps[1]}, nil
}

// ancestorChain returns an issue's parentage, self first and root last,
// following only parents that are still there. A parent cycle is corrupt data
// and fails loudly rather than looping. [LAW:no-silent-failure]
func (e *Engine) ancestorChain(id string) ([]string, error) {
	if _, err := e.mustRecord(id); err != nil {
		return nil, err
	}
	chain := []string{id}
	seen := map[string]struct{}{id: {}}
	for current := id; ; {
		parent, ok := e.parentOf(current)
		if !ok {
			return chain, nil
		}
		if _, looped := seen[parent]; looped {
			return nil, fmt.Errorf("ancestor chain of %s: parent cycle at %s", id, parent)
		}
		seen[parent] = struct{}{}
		chain = append(chain, parent)
		current = parent
	}
}

// parentOf names an issue's parent, skipping a parent that has been deleted:
// a frame is what an issue is ranked within, and work in the trash frames
// nothing.
func (e *Engine) parentOf(childID string) (string, bool) {
	for _, rel := range e.relations {
		if rel.Type != model.RelParentChild || rel.SrcID != childID {
			continue
		}
		parent, ok := e.issues[rel.DstID]
		if !ok {
			continue
		}
		if _, gone := parent.retention.(model.Deleted); gone {
			continue
		}
		return rel.DstID, true
	}
	return "", false
}

// frameContainmentError reports a rank request naming an issue together with
// one of its own ancestors. No comparable frame holds both, so no
// frame-coherent order between them exists.
type frameContainmentError struct {
	containerID string
	containedID string
}

func (e *frameContainmentError) Error() string {
	return fmt.Sprintf("%s is inside %s; no comparable frame contains both — rank it against a sibling instead", e.containedID, e.containerID)
}

// frameRepresentatives maps each ancestor chain onto its stand-in in the
// chains' comparable frame.
//
// Rank meaning is frame-local: an issue's rank is only ever read against its
// frame-mates, so issues from different frames resolve to their representatives
// directly under the lowest common ancestor of all the chains — and to their
// roots when the ancestries share none, the top level being the frame that
// contains everything. Nothing inside any epic is reordered by a cross-frame
// request. [LAW:types-are-the-program] A cross-frame position is an illegal
// state of the order; this resolution is what makes every write a legal one.
func frameRepresentatives(chains [][]string) ([]string, error) {
	depths := make([]map[string]int, len(chains))
	for i, chain := range chains {
		depth := make(map[string]int, len(chain))
		for index, id := range chain {
			depth[id] = index
		}
		depths[i] = depth
	}
	for i, chain := range chains {
		for j, depth := range depths {
			if i == j {
				continue
			}
			if index, inside := depth[chain[0]]; inside && index > 0 {
				return nil, &frameContainmentError{containerID: chain[0], containedID: chains[j][0]}
			}
		}
	}
	// Common ancestors form a shared suffix of every chain, so the first
	// element of the first chain present in all the others is the lowest
	// common ancestor.
	lowestCommon := ""
	for _, id := range chains[0] {
		shared := true
		for _, depth := range depths[1:] {
			if _, ok := depth[id]; !ok {
				shared = false
				break
			}
		}
		if shared {
			lowestCommon = id
			break
		}
	}
	reps := make([]string, len(chains))
	for i, chain := range chains {
		if lowestCommon == "" {
			reps[i] = chain[len(chain)-1]
			continue
		}
		reps[i] = chain[depths[i][lowestCommon]-1]
	}
	return reps, nil
}
