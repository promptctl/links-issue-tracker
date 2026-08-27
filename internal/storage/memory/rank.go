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

// RankToTop and RankToBottom name the two ends of the order directly, so they
// need no anchor and no frame: every issue is comparable with the ends.
func (e *Engine) RankToTop(ctx context.Context, issueID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rankToEnd(issueID, storage.RankTop)
}

func (e *Engine) RankToBottom(ctx context.Context, issueID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rankToEnd(issueID, storage.RankBottom)
}

func (e *Engine) rankToEnd(issueID string, end storage.RankPlacement) error {
	if _, err := e.mustRecord(issueID); err != nil {
		return err
	}
	e.detach(issueID)
	e.place(issueID, end)
	return nil
}

// RankSet imposes a total order on the named issues at once, stacking them at
// the top of the order in the order named, and reports which representative
// each name resolved to.
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
	for _, rep := range reps {
		e.detach(rep)
	}
	e.order = append(slices.Clone(reps), e.order...)
	return resolutions, nil
}

// detach lifts an id out of the order, leaving the rest of the sequence
// intact. Every rank verb is a detach followed by a placement, which is what
// makes "the order after the move" a fact about the slice rather than a
// consequence of arithmetic on keys.
func (e *Engine) detach(id string) {
	e.order = slices.DeleteFunc(e.order, func(existing string) bool { return existing == id })
}

func (e *Engine) insertAt(index int, id string) {
	e.order = slices.Insert(e.order, min(max(index, 0), len(e.order)), id)
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
	if _, err := e.mustRecord(targetID); err != nil {
		return storage.RankMove{}, err
	}
	if _, err := e.mustRecord(issueID); err != nil {
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
