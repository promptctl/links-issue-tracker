package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/rank"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// frameColumn is the SQL scalar for an issue's frame: the id of its live
// container, or the empty string — storage.TopLevel — for an issue no
// container holds. Every rank neighbor lookup is scoped with it, which is what
// confines a written rank to the keyspace it will actually be read in.
//
// A deleted container frames nothing, so the join drops those rows and an
// orphan falls back to the top level — the same rule ancestorChain walks by,
// spelled for the query planner. [LAW:one-source-of-truth] The two must agree
// about what contains what; this is that rule's SQL rendering, not a second
// rule. The expression correlates on issues.id, so it belongs only in a query
// selecting FROM issues.
const frameColumn = `COALESCE((SELECT r.dst_id FROM relations r
		JOIN issues p ON p.id = r.dst_id
		WHERE r.src_id = issues.id AND r.type = 'parent-child' AND p.deleted_at IS NULL), '')`

// rankEdge is one end of a frame's keyspace as the query and the key algebra
// see it: the ordering that brings that end to the front of a result, the step
// that lands just past it, and the word for it. The end a caller asked for
// crosses as this value, so one statement and one assignment serve both ends
// instead of two near-copies free to drift.
// [LAW:dataflow-not-control-flow]
type rankEdge struct {
	name   string
	order  string
	beyond func(string) string
}

// edgeFor is the single dispatch point on RankPlacement for rank keys: both
// edge verbs and issue creation's placement resolve their end here, so there
// is no second opinion about which direction "top" is.
// [LAW:single-enforcer]
func edgeFor(p storage.RankPlacement) (rankEdge, error) {
	switch p {
	case storage.RankTop:
		return rankEdge{name: "top", order: "ASC", beyond: rank.Before}, nil
	case storage.RankBottom:
		return rankEdge{name: "bottom", order: "DESC", beyond: rank.After}, nil
	default:
		return rankEdge{}, fmt.Errorf("unknown rank placement: %d", p)
	}
}

// rankBeyond is the key just past a frame's edge — or the frame's first key
// when the frame holds nothing ranked yet. Absorbing the empty frame here is
// what lets every caller assign unconditionally instead of repeating the same
// "is there anything to anchor against" branch.
// [LAW:dataflow-not-control-flow]
func (e rankEdge) rankBeyond(edgeRank string) string {
	if edgeRank == "" {
		return rank.Initial()
	}
	return e.beyond(edgeRank)
}

// frameEdgeHolderTx names the issue holding one end of a frame's rank order
// and the rank it holds there. Both come back empty when the frame has nothing
// ranked in it — a fact about the frame, not a failure, and the one input
// rankBeyond needs to seed a fresh frame.
func frameEdgeHolderTx(ctx context.Context, tx *sql.Tx, f storage.Frame, edge rankEdge) (holderID, holderRank string, err error) {
	query := fmt.Sprintf(`SELECT id, item_rank FROM issues
		WHERE deleted_at IS NULL AND item_rank != '' AND %s = ?
		ORDER BY item_rank %s LIMIT 1`, frameColumn, edge.order)
	err = tx.QueryRowContext(ctx, query, string(f)).Scan(&holderID, &holderRank)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("query %s of frame %q: %w", edge.name, f, err)
	}
	return holderID, holderRank, nil
}

// frameOfTx names the frame an issue's own rank lives in: its container, or the
// top level. It reads inside the caller's transaction so the frame a write is
// scoped to is established under the same commit lock as the write itself.
//
// Built on frameColumn, the one SQL rendering of what contains what, so this
// cannot drift from the rule every neighbor lookup is already scoped by.
// [LAW:one-source-of-truth]
func frameOfTx(ctx context.Context, tx *sql.Tx, issueID string) (storage.Frame, error) {
	query := fmt.Sprintf(`SELECT %s FROM issues WHERE id = ? AND deleted_at IS NULL`, frameColumn)
	var f string
	if err := tx.QueryRowContext(ctx, query, issueID).Scan(&f); err != nil {
		return storage.TopLevel, fmt.Errorf("frame of %s: %w", issueID, err)
	}
	return storage.Frame(f), nil
}

// RankToTop moves an issue to the top of its own frame.
func (s *Store) RankToTop(ctx context.Context, issueID string) (storage.RankEnd, error) {
	return s.rankToEdge(ctx, issueID, storage.RankTop)
}

// RankToBottom moves an issue to the bottom of its own frame.
func (s *Store) RankToBottom(ctx context.Context, issueID string) (storage.RankEnd, error) {
	return s.rankToEdge(ctx, issueID, storage.RankBottom)
}

// rankToEdge moves an issue to one end of its own frame.
//
// The end is a frame's end, never the workspace's. A child ranked to the top
// leads its siblings, and seeding its key from the globally-first rank would
// draw that key from a keyspace shared with issues it is never compared
// against — which is how one epic's edits came to displace the top level's
// keys, and how the next top-level promotion came to be computed against a
// child. Scoping the lookup keeps the written key comparable only to the keys
// it is actually read against. [LAW:types-are-the-program]
//
// [LAW:single-enforcer] Both edge verbs take their key from
// frameEdgeHolderTx, so there is no second notion of the rank at a frame's
// edge. Creation asks a deliberately different question — the edge of the
// whole workspace, see nextRankForPlacement — and shares the direction and the
// empty-keyspace default through edgeFor rather than its own copy of them.
func (s *Store) rankToEdge(ctx context.Context, issueID string, placement storage.RankPlacement) (storage.RankEnd, error) {
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return storage.RankEnd{}, err
	}
	edge, err := edgeFor(placement)
	if err != nil {
		return storage.RankEnd{}, err
	}
	var result storage.RankEnd
	// Both the frame and its edge are read inside the mutation, under the same
	// commit lock as the write they seed. Reading the frame before the lock
	// would leave a window in which a concurrent reparent moves the issue: the
	// edge lookup would then be scoped to a container the issue has already
	// left, and the key written would be drawn from a keyspace it is never read
	// against — this bug, reached through timing instead of through arithmetic.
	// [LAW:no-ambient-temporal-coupling]
	err = s.withMutation(ctx, "rank to "+edge.name, func(ctx context.Context, tx *sql.Tx) error {
		f, err := frameOfTx(ctx, tx, issueID)
		if err != nil {
			return err
		}
		result.Frame = f
		holderID, holderRank, err := frameEdgeHolderTx(ctx, tx, f, edge)
		if err != nil {
			return err
		}
		// An issue already holding its frame's edge has nowhere to go. Writing
		// its own rank back would bump updated_at and report a promotion that
		// never happened; the caller is told instead. [LAW:no-silent-failure]
		result.Moved = holderID != issueID
		if !result.Moved {
			return nil
		}
		newRank := edge.rankBeyond(holderRank)
		now := s.clock.Now().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, issueID); err != nil {
			return fmt.Errorf("rank to %s: update: %w", edge.name, err)
		}
		return smoothRanksIfNeededTx(ctx, tx, newRank)
	})
	return result, err
}

func rankSetValidateIDs(ids []string) error {
	if len(ids) < 2 {
		return errors.New("rank set: need at least 2 IDs to establish order")
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			return errors.New("rank set: empty ID in input")
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("rank set: duplicate ID %q in input", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// resolveRankSet maps the named IDs onto the frame-comparable representatives
// a rank-set order is actually about (see resolveFrameRepresentatives). Two
// named IDs collapsing onto one representative is rejected: the requested
// total order places issues from inside one epic relative to outsiders, which
// no frame-coherent write can express — honoring part of the order while
// silently discarding the rest would misrepresent the request.
// [LAW:no-silent-failure]
func (s *Store) resolveRankSet(ctx context.Context, ids []string) ([]storage.RankSetResolution, storage.Frame, error) {
	chains := make([][]string, len(ids))
	for i, id := range ids {
		if _, err := s.GetIssue(ctx, id); err != nil {
			return nil, storage.TopLevel, err
		}
		chain, err := s.ancestorChain(ctx, id)
		if err != nil {
			return nil, storage.TopLevel, err
		}
		chains[i] = chain
	}
	reps, f, err := resolveFrameRepresentatives(chains)
	if err != nil {
		return nil, storage.TopLevel, fmt.Errorf("rank set: %w", err)
	}
	resolutions := make([]storage.RankSetResolution, len(ids))
	repToNamed := make(map[string]string, len(ids))
	for i, id := range ids {
		if prior, dup := repToNamed[reps[i]]; dup {
			return nil, storage.TopLevel, fmt.Errorf("rank set: %s and %s both resolve to %s — their relative order is internal to %s and cannot be set against outside issues; run rank set among siblings instead", prior, id, reps[i], reps[i])
		}
		repToNamed[reps[i]] = id
		resolutions[i] = storage.RankSetResolution{NamedID: id, RankedID: reps[i]}
	}
	return resolutions, f, nil
}

// RankSet establishes absolute order across the given IDs by stacking them at
// the top of the representatives' own frame in the order supplied: ids[0]
// becomes topmost, ids[1] ranks just below, etc. IDs are first resolved to
// their frame-comparable representatives (a child's stand-in is its epic), so
// the order written is always frame-coherent and nothing inside any epic is
// reordered. The anchor is that frame's top, never the workspace's: setting an
// order among one epic's children leads that epic's children and moves nothing
// outside it. Atomic — every assignment commits together or none does.
// Validates IDs exist and rejects duplicates before any write.
// [LAW:single-enforcer] Multi-issue rank reassignment lives in this one
// transaction so partial-application states cannot occur.
func (s *Store) RankSet(ctx context.Context, ids []string) ([]storage.RankSetResolution, error) {
	if err := rankSetValidateIDs(ids); err != nil {
		return nil, err
	}
	resolutions, f, err := s.resolveRankSet(ctx, ids)
	if err != nil {
		return nil, err
	}
	ranked := make([]string, len(resolutions))
	for i, r := range resolutions {
		ranked[i] = r.RankedID
	}
	return resolutions, s.withMutation(ctx, "rank set", func(ctx context.Context, tx *sql.Tx) error {
		// Find the topmost rank in the representatives' own frame, excluding the
		// IDs being reassigned (so we anchor against rows that aren't moving).
		// Every representative is a frame-mate by construction, so that frame is
		// the whole keyspace this stack is read in.
		args := make([]any, 0, len(ranked)+1)
		placeholders := make([]string, 0, len(ranked))
		for _, id := range ranked {
			args = append(args, id)
			placeholders = append(placeholders, "?")
		}
		args = append(args, string(f))
		query := fmt.Sprintf(`SELECT item_rank FROM issues
			WHERE deleted_at IS NULL AND item_rank != '' AND id NOT IN (%s) AND %s = ?
			ORDER BY item_rank ASC LIMIT 1`, strings.Join(placeholders, ","), frameColumn)
		var topRank sql.NullString
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&topRank); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("rank-set: query top: %w", err)
		}

		// Walk IDs in reverse, assigning each a rank just above the previous one.
		// The last ID (idx N-1) is anchored just above the existing top; each
		// earlier ID is anchored just above the previously-assigned rank, so the
		// final order is ids[0] < ids[1] < ... < ids[N-1] < (existing top).
		now := s.clock.Now().Format(time.RFC3339Nano)
		cursor := topRank.String
		hasCursor := topRank.Valid && topRank.String != ""
		newRanks := make([]string, len(ranked))
		for i := len(ranked) - 1; i >= 0; i-- {
			var newRank string
			if !hasCursor {
				newRank = rank.Initial()
				hasCursor = true
			} else {
				newRank = rank.Before(cursor)
			}
			newRanks[i] = newRank
			cursor = newRank
		}
		for i, id := range ranked {
			if _, err := tx.ExecContext(ctx, `UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?`, newRanks[i], now, id); err != nil {
				return fmt.Errorf("rank-set: update %s: %w", id, err)
			}
		}
		if len(newRanks) > 0 {
			return smoothRanksIfNeededTx(ctx, tx, newRanks[0])
		}
		return nil
	})
}

// ancestorChain returns the parent-child ancestry of an issue, self first,
// root last, following only non-deleted parents. The on-disk relation rows
// are a trust boundary: a parent cycle is corrupt data and fails loudly
// rather than looping. [LAW:no-silent-failure]
func (s *Store) ancestorChain(ctx context.Context, id string) ([]string, error) {
	chain := []string{id}
	seen := map[string]struct{}{id: {}}
	for cur := id; ; {
		var parent string
		err := s.db.QueryRowContext(ctx,
			`SELECT r.dst_id FROM relations r JOIN issues p ON p.id = r.dst_id
			 WHERE r.src_id = ? AND r.type = 'parent-child' AND p.deleted_at IS NULL`, cur).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return chain, nil
		}
		if err != nil {
			return nil, fmt.Errorf("ancestor chain of %s: %w", id, err)
		}
		if _, ok := seen[parent]; ok {
			return nil, fmt.Errorf("ancestor chain of %s: parent cycle at %s", id, parent)
		}
		seen[parent] = struct{}{}
		chain = append(chain, parent)
		cur = parent
	}
}

// frameContainmentError reports a rank request that names an issue together
// with one of its own ancestors. No comparable frame holds both — the
// contained issue's representative would be (or sit under) the container
// itself — so no frame-coherent rank order between them exists.
type frameContainmentError struct {
	containerID string
	containedID string
}

func (e *frameContainmentError) Error() string {
	return fmt.Sprintf("%s is inside %s; no comparable frame contains both — rank it against a sibling instead", e.containedID, e.containerID)
}

// resolveFrameRepresentatives maps each ancestor chain (self first, root
// last) onto its representative in the chains' comparable frame. Rank meaning
// is frame-local: an issue's rank is only ever compared against its
// frame-mates (siblings under the same container, or fellow top-level items),
// so issues from different frames resolve to their representatives directly
// under the lowest common ancestor of all chains — and to their roots when
// the ancestries share none, the top level being the comparable frame.
// Nothing inside any epic is ever reordered by a cross-frame request.
//
// The frame the representatives live in comes back with them: it is the lowest
// common ancestor the walk already found, and it is the scope every neighbor
// lookup seeded by these representatives must be taken in. Returning it here
// rather than re-deriving it at each callsite keeps the representatives and
// their keyspace from ever disagreeing. [LAW:one-source-of-truth]
//
// [LAW:types-are-the-program] Cross-frame midpoints are an illegal state of
// the rank keyspace; this resolution makes every write frame-coherent.
// [LAW:single-enforcer] The one resolution core behind every rank verb.
func resolveFrameRepresentatives(chains [][]string) ([]string, storage.Frame, error) {
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
				return nil, storage.TopLevel, &frameContainmentError{containerID: chain[0], containedID: chains[j][0]}
			}
		}
	}
	// Common ancestors form a shared suffix of every chain, so the first
	// element of chains[0] present in all others is the lowest common ancestor.
	lcaID := ""
	for _, id := range chains[0] {
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
	// holding every root is the top level — which storage.Frame spells as the
	// same empty string lcaID already carries.
	return reps, storage.Frame(lcaID), nil
}

// resolveComparableFrame maps a relative rank request onto the pair it is
// actually about: ranking a standalone ticket against an epic's child behaves
// as ranking against the epic itself, and ranking the child against the
// standalone moves the epic (see resolveFrameRepresentatives). Ranking an
// issue relative to its own container (or descendant) has no frame-coherent
// meaning and is rejected. [LAW:no-silent-failure]
func resolveComparableFrame(issueChain, targetChain []string) (movedID, anchorID string, f storage.Frame, err error) {
	reps, f, err := resolveFrameRepresentatives([][]string{issueChain, targetChain})
	var containment *frameContainmentError
	if errors.As(err, &containment) {
		if containment.containerID == issueChain[0] {
			return "", "", storage.TopLevel, fmt.Errorf("cannot rank %s relative to %s: %s contains it; rank it against a sibling instead", issueChain[0], targetChain[0], issueChain[0])
		}
		return "", "", storage.TopLevel, fmt.Errorf("cannot rank %s relative to %s: %s is inside %s; rank it against a sibling instead", issueChain[0], targetChain[0], issueChain[0], targetChain[0])
	}
	if err != nil {
		return "", "", storage.TopLevel, err
	}
	return reps[0], reps[1], f, nil
}

// resolveRankPair validates a relative rank request and resolves it to the
// frame-comparable pair, returning the hydrated anchor (its rank seeds the
// midpoint math), the move record, and the frame both representatives live in
// — which is the scope the midpoint's neighbor must be drawn from.
// [LAW:single-enforcer] Both relative rank ops route through this one
// resolution so cross-frame semantics cannot drift between above and below.
func (s *Store) resolveRankPair(ctx context.Context, issueID, targetID string) (model.Issue, storage.RankMove, storage.Frame, error) {
	if issueID == targetID {
		return model.Issue{}, storage.RankMove{}, storage.TopLevel, errors.New("cannot rank an issue relative to itself")
	}
	if _, err := s.GetIssue(ctx, targetID); err != nil {
		return model.Issue{}, storage.RankMove{}, storage.TopLevel, err
	}
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return model.Issue{}, storage.RankMove{}, storage.TopLevel, err
	}
	issueChain, err := s.ancestorChain(ctx, issueID)
	if err != nil {
		return model.Issue{}, storage.RankMove{}, storage.TopLevel, err
	}
	targetChain, err := s.ancestorChain(ctx, targetID)
	if err != nil {
		return model.Issue{}, storage.RankMove{}, storage.TopLevel, err
	}
	movedID, anchorID, f, err := resolveComparableFrame(issueChain, targetChain)
	if err != nil {
		return model.Issue{}, storage.RankMove{}, storage.TopLevel, err
	}
	anchor, err := s.GetIssue(ctx, anchorID)
	if err != nil {
		return model.Issue{}, storage.RankMove{}, storage.TopLevel, err
	}
	return anchor, storage.RankMove{MovedID: movedID, AnchorID: anchorID}, f, nil
}

// RankAbove moves an issue to rank immediately above the target issue,
// after resolving both to their comparable frame (see resolveComparableFrame).
func (s *Store) RankAbove(ctx context.Context, issueID, targetID string) (storage.RankMove, error) {
	target, move, f, err := s.resolveRankPair(ctx, issueID, targetID)
	if err != nil {
		return storage.RankMove{}, err
	}
	return move, s.withMutation(ctx, "rank above", func(ctx context.Context, tx *sql.Tx) error {
		// The neighbor is drawn from the anchor's frame, not the workspace: a
		// closer key belonging to some other epic's child would still land the
		// issue above its anchor, but it would halve the gap against a row this
		// order never reads, spending precision and mixing the keyspaces for
		// nothing. [LAW:types-are-the-program]
		var aboveRank sql.NullString
		query := fmt.Sprintf(`SELECT item_rank FROM issues
			WHERE item_rank < ? AND deleted_at IS NULL AND id != ? AND %s = ?
			ORDER BY item_rank DESC LIMIT 1`, frameColumn)
		err := tx.QueryRowContext(ctx, query, target.Rank, move.MovedID, string(f)).Scan(&aboveRank)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("rank-above: query neighbor: %w", err)
		}
		var newRank string
		if !aboveRank.Valid || aboveRank.String == "" {
			newRank = rank.Before(target.Rank)
		} else {
			newRank, err = rank.Midpoint(aboveRank.String, target.Rank)
			if err != nil {
				return fmt.Errorf("rank-above: midpoint: %w", err)
			}
		}
		now := s.clock.Now().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, move.MovedID); err != nil {
			return fmt.Errorf("rank-above: update: %w", err)
		}
		return smoothRanksIfNeededTx(ctx, tx, newRank)
	})
}

// RankBelow moves an issue to rank immediately below the target issue,
// after resolving both to their comparable frame (see resolveComparableFrame).
func (s *Store) RankBelow(ctx context.Context, issueID, targetID string) (storage.RankMove, error) {
	target, move, f, err := s.resolveRankPair(ctx, issueID, targetID)
	if err != nil {
		return storage.RankMove{}, err
	}
	return move, s.withMutation(ctx, "rank below", func(ctx context.Context, tx *sql.Tx) error {
		// Scoped to the anchor's frame for the same reason RankAbove is: the
		// midpoint is only meaningful against a key this order actually reads.
		var belowRank sql.NullString
		query := fmt.Sprintf(`SELECT item_rank FROM issues
			WHERE item_rank > ? AND deleted_at IS NULL AND id != ? AND %s = ?
			ORDER BY item_rank ASC LIMIT 1`, frameColumn)
		err := tx.QueryRowContext(ctx, query, target.Rank, move.MovedID, string(f)).Scan(&belowRank)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("rank-below: query neighbor: %w", err)
		}
		var newRank string
		if !belowRank.Valid || belowRank.String == "" {
			newRank = rank.After(target.Rank)
		} else {
			newRank, err = rank.Midpoint(target.Rank, belowRank.String)
			if err != nil {
				return fmt.Errorf("rank-below: midpoint: %w", err)
			}
		}
		now := s.clock.Now().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, move.MovedID); err != nil {
			return fmt.Errorf("rank-below: update: %w", err)
		}
		return smoothRanksIfNeededTx(ctx, tx, newRank)
	})
}

// smoothRanksIfNeededTx checks whether the given rank string has grown past the
// smoothing threshold and, if so, re-spaces a local window of items around the
// insertion point, at O(SmoothingWindow) cost instead of a full O(n) rebalance.
// Ranks sharing a significant part — one being the other extended by zeros —
// pad to the same value and so leave no room between them, and they sort
// contiguously. The window therefore swallows the whole run its ends sit in, so
// a run of zero-extensions widens it past SmoothingWindow, and each bound is
// taken from outside that run: below the run's own least rank, and at or above
// the least rank sorting past every member of the top run. Bounds picked that
// way can never share a significant part, so the primitive always has room.
// The window keeps its order, and its new ranks are longer than both bounds,
// so a window bounded by long ranks comes out long.
func smoothRanksIfNeededTx(ctx context.Context, tx *sql.Tx, triggerRank string) error {
	if len(triggerRank) < rank.SmoothingThreshold {
		return nil
	}
	half := rank.SmoothingWindow / 2

	// Collect the window: up to half items at or below the trigger, plus
	// up to half items above it.
	below, err := rankRowsTx(ctx, tx,
		`SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank <= ? ORDER BY item_rank DESC LIMIT ?`,
		triggerRank, half)
	if err != nil {
		return fmt.Errorf("smooth: below: %w", err)
	}
	slices.Reverse(below)

	above, err := rankRowsTx(ctx, tx,
		`SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank > ? ORDER BY item_rank ASC LIMIT ?`,
		triggerRank, half)
	if err != nil {
		return fmt.Errorf("smooth: above: %w", err)
	}
	window := slices.Concat(below, above)

	if len(window) < 2 {
		return nil
	}

	// [LAW:one-source-of-truth] rank.Significant is the definition of room, as in
	// anchorRun, so the window is widened by it rather than by raw adjacency.
	// runFloor is the least rank sharing the window's bottom significant part and
	// runCeiling the least rank sorting above every rank sharing its top one, so
	// each bound below is addressed directly rather than scanned for.
	runFloor := rank.Significant(window[0].rank)
	runCeiling := rank.Significant(window[len(window)-1].rank) + "1"

	lowerRun, err := rankRowsTx(ctx, tx,
		`SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank >= ? AND item_rank < ? ORDER BY item_rank ASC`,
		runFloor, window[0].rank)
	if err != nil {
		return fmt.Errorf("smooth: lower run: %w", err)
	}
	upperRun, err := rankRowsTx(ctx, tx,
		`SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank > ? AND item_rank < ? ORDER BY item_rank ASC`,
		window[len(window)-1].rank, runCeiling)
	if err != nil {
		return fmt.Errorf("smooth: upper run: %w", err)
	}
	window = slices.Concat(lowerRun, window, upperRun)

	lowerBound, err := nearestRankTx(ctx, tx,
		`SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank < ? ORDER BY item_rank DESC LIMIT 1`,
		runFloor)
	if err != nil {
		return fmt.Errorf("smooth: lower bound: %w", err)
	}
	upperBound, err := nearestRankTx(ctx, tx,
		`SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank >= ? ORDER BY item_rank ASC LIMIT 1`,
		runCeiling)
	if err != nil {
		return fmt.Errorf("smooth: upper bound: %w", err)
	}

	newRanks, err := rank.SpacedRanksBetween(lowerBound, upperBound, len(window))
	if err != nil {
		return fmt.Errorf("smooth: compute ranks: %w", err)
	}

	for i, item := range window {
		if newRanks[i] != item.rank {
			if _, err := tx.ExecContext(ctx, `UPDATE issues SET item_rank = ? WHERE id = ?`, newRanks[i], item.id); err != nil {
				return fmt.Errorf("smooth: update %s: %w", item.id, err)
			}
		}
	}
	return nil
}

// rankRowsTx runs a rank query and returns every row it matches, in the order
// the query asks for. Every caller bounds its own query — by range or by LIMIT
// — so the rows read stay proportional to the window, never to the backlog.
func rankRowsTx(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]rankedIssue, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rankedIssue
	for rows.Next() {
		var item rankedIssue
		if err := rows.Scan(&item.id, &item.rank); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// nearestRankTx returns the single rank a LIMIT 1 query selects, or "" — the
// open end of the keyspace — when it selects nothing.
func nearestRankTx(ctx context.Context, tx *sql.Tx, query string, args ...any) (string, error) {
	var found string
	err := tx.QueryRowContext(ctx, query, args...).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return found, nil
}

// rowQueryer abstracts the QueryContext surface that *sql.DB and *sql.Tx
// share, so one loader serves Doctor (reading, no tx) and FixRankInversions
// (reading inside its own mutating tx).
// [LAW:single-enforcer] Each thing the two need to read — the blocks edges and
// the live rank order — is loaded by exactly one function taking this
// interface, so neither caller can read a different store than the other.
type rowQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// liveIssueIDs returns the set of non-archived, non-deleted issue IDs whose
// lifecycle State() is not Closed. Archived issues are user-deprioritized and
// do not generate actionable inversions, for the same reason closed issues do
// not.
//
// The classification is settled in Go, never in SQL, and that is load-bearing.
// Epics store status=NULL by design — the issues_status_check constraint in
// migrations/00001_baseline.sql encodes it: epics have status IS NULL, leaves
// carry a known value — and derive their state from their children via AllOf.
// So a SQL-side `status != 'closed'` test evaluates to NULL rather than TRUE
// for every epic and silently drops every blocks-edge pointing at one, which is
// exactly the bug that once had Doctor reporting zero inversions while ready.go
// flagged the same edge. Hydrating through the canonical path instead gets the
// AllOf rollup, never a raw column peek that would lie about an epic.
// [LAW:one-source-of-truth] State classification rides the lifecycle here,
// the same predicate ready uses for its rank_inversion annotations.
func (s *Store) liveIssueIDs(ctx context.Context) (map[string]struct{}, error) {
	issues, err := s.ListIssues(ctx, storage.ListIssuesFilter{Statuses: []model.State{model.StateOpen, model.StateInProgress}})
	if err != nil {
		return nil, fmt.Errorf("list live issues: %w", err)
	}
	out := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		out[issue.ID] = struct{}{}
	}
	return out, nil
}

// blocksEdge is one blocks relation hydrated as a precedence constraint: the
// dependent must be ranked below the dependency, i.e. the dependency comes
// first. A rank order is a total order over issues, and a total order that
// satisfies every blocks edge exists if and only if the precedence graph is
// acyclic. A cycle is therefore not a transient rank stall but an
// unsatisfiable constraint set — no assignment can place every dependency
// above its dependent once the dependencies form a loop.
type blocksEdge struct {
	dependent  string // src — ranked below
	dependency string // dst — ranked above
}

// loadBlocksEdges returns every blocks relation whose endpoints are both
// non-deleted. It does not filter on rank: the constraint graph is what it is
// whatever the current ranks say, and every caller asks about the graph rather
// than about placement.
func loadBlocksEdges(ctx context.Context, q rowQueryer) ([]blocksEdge, error) {
	// ORDER BY makes edge iteration — and therefore the adjacency order that
	// findBlocksCycle's DFS follows — stable across runs and engines, so the
	// reported cycle path is deterministic.
	rows, err := q.QueryContext(ctx, `SELECT r.src_id, r.dst_id FROM relations r
		JOIN issues src ON src.id = r.src_id
		JOIN issues dst ON dst.id = r.dst_id
		WHERE r.type = 'blocks'
		AND src.deleted_at IS NULL AND dst.deleted_at IS NULL
		ORDER BY r.src_id, r.dst_id`)
	if err != nil {
		return nil, fmt.Errorf("query blocks edges: %w", err)
	}
	defer rows.Close()
	edges := make([]blocksEdge, 0)
	for rows.Next() {
		var e blocksEdge
		if err := rows.Scan(&e.dependent, &e.dependency); err != nil {
			return nil, fmt.Errorf("scan blocks edge: %w", err)
		}
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blocks edges rows: %w", err)
	}
	return edges, nil
}

// blocksPrecedenceAdj builds the precedence adjacency dependency -> []dependent.
func blocksPrecedenceAdj(edges []blocksEdge) map[string][]string {
	adj := make(map[string][]string, len(edges))
	for _, e := range edges {
		adj[e.dependency] = append(adj[e.dependency], e.dependent)
	}
	return adj
}

// blocksPrecedes reports whether `from` already precedes `to` through a chain
// of blocks edges — i.e. existing relations already force `from` to be ranked
// ahead of `to`. Adding a `to`-precedes-`from` edge on top of that would close
// a cycle.
func blocksPrecedes(adj map[string][]string, from, to string) bool {
	seen := make(map[string]struct{})
	var walk func(string) bool
	walk = func(n string) bool {
		for _, next := range adj[n] {
			if next == to {
				return true
			}
			if _, ok := seen[next]; ok {
				continue
			}
			seen[next] = struct{}{}
			if walk(next) {
				return true
			}
		}
		return false
	}
	return walk(from)
}

// findBlocksCycle returns one cycle in the blocks precedence graph as an
// ordered, repeated-endpoint path (a -> b -> ... -> a), or nil when the graph
// is acyclic. Node iteration is sorted so the reported cycle is deterministic.
func findBlocksCycle(edges []blocksEdge) []string {
	adj := blocksPrecedenceAdj(edges)
	nodes := make([]string, 0, len(adj))
	for n := range adj {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int)
	var stack []string
	var dfs func(string) []string
	dfs = func(n string) []string {
		color[n] = gray
		stack = append(stack, n)
		for _, m := range adj[n] {
			switch color[m] {
			case gray:
				for i, s := range stack {
					if s == m {
						return append(append([]string(nil), stack[i:]...), m)
					}
				}
			case white:
				if c := dfs(m); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return nil
	}
	for _, n := range nodes {
		if color[n] == white {
			if c := dfs(n); c != nil {
				return c
			}
		}
	}
	return nil
}

// loadRankOrder returns the live issues in the order the store ranks them:
// item_rank ascending, with id breaking ties so the sequence is total even
// where two rows share a rank. This sequence is both the repair's input order
// and its membership test for which blocks edges constrain live work.
func loadRankOrder(ctx context.Context, q rowQueryer, liveIDs map[string]struct{}) ([]rankedIssue, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, item_rank FROM issues WHERE deleted_at IS NULL ORDER BY item_rank ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query rank order: %w", err)
	}
	defer rows.Close()
	order := make([]rankedIssue, 0, len(liveIDs))
	for rows.Next() {
		var item rankedIssue
		if err := rows.Scan(&item.id, &item.rank); err != nil {
			return nil, fmt.Errorf("scan rank order: %w", err)
		}
		if _, live := liveIDs[item.id]; !live {
			continue
		}
		order = append(order, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rank order rows: %w", err)
	}
	return order, nil
}

// FixRankInversions re-ranks the backlog so every dependency outranks its
// dependent. It computes the stable topological order of the live rank
// sequence (see repairRankOrder), rewrites the issues that order moved, and
// returns how many it moved.
//
// [LAW:single-enforcer] Doctor's rank_inversions count and this repair read the
// same blocks edges over the same live set, so "no edge is inverted" is one
// predicate seen twice: the count reports the edges that break it, and the
// order this produces satisfies it by construction.
func (s *Store) FixRankInversions(ctx context.Context) (int, error) {
	// Liveness is computed once before the tx: the repair only mutates
	// item_rank, so closure status is invariant across the write. Re-classifying
	// inside the tx would require plumbing the queryer through hydrateIssues;
	// the snapshot semantics are equivalent and simpler.
	// [LAW:dataflow-not-control-flow] Liveness is data the repair reads, not a
	// branch it takes.
	liveIDs, err := s.liveIssueIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("fix rank inversions: snapshot live set: %w", err)
	}
	rerankedCount := 0
	if err := s.withMutation(ctx, "fix rank inversions", func(ctx context.Context, tx *sql.Tx) error {
		order, err := loadRankOrder(ctx, tx, liveIDs)
		if err != nil {
			return fmt.Errorf("fix rank inversions: %w", err)
		}
		edges, err := loadBlocksEdges(ctx, tx)
		if err != nil {
			return fmt.Errorf("fix rank inversions: load blocks edges: %w", err)
		}
		rewrites, err := repairRankOrder(order, edges)
		if err != nil {
			return fmt.Errorf("fix rank inversions: %w", err)
		}
		now := s.clock.Now().Format(time.RFC3339Nano)
		for _, rewrite := range rewrites {
			if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", rewrite.newRank, now, rewrite.id); err != nil {
				return fmt.Errorf("fix rank inversions: update %s: %w", rewrite.id, err)
			}
		}
		// Smoothing runs once the repair is fully applied, never interleaved
		// with it: a pass that re-spaced a window mid-repair would move the
		// anchor ranks the remaining placements were computed against. It
		// preserves relative order, so it never undoes the repair and moves no
		// issue; the ranks it re-spaces, anchors included, are not counted.
		for _, rewrite := range rewrites {
			if err := smoothRanksIfNeededTx(ctx, tx, rewrite.newRank); err != nil {
				return fmt.Errorf("fix rank inversions: smooth ranks: %w", err)
			}
		}
		// Assigned, never accumulated: withStampedMutation may re-run this
		// function after a transient failure rolls its writes back, and a
		// running total would count the discarded attempt too.
		rerankedCount = len(rewrites)
		return nil
	}); err != nil {
		return 0, err
	}
	return rerankedCount, nil
}
