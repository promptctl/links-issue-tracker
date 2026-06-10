package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/rank"
)

func (s *Store) RankToTop(ctx context.Context, issueID string) error {
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return err
	}
	return s.withMutation(ctx, "rank to top", func(ctx context.Context, tx *sql.Tx) error {
		var firstRank sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' AND id != ? ORDER BY item_rank ASC LIMIT 1", issueID).Scan(&firstRank)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("rank-to-top: query first: %w", err)
		}
		var newRank string
		if !firstRank.Valid || firstRank.String == "" {
			newRank = rank.Initial()
		} else {
			newRank = rank.Before(firstRank.String)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, issueID); err != nil {
			return fmt.Errorf("rank-to-top: update: %w", err)
		}
		return smoothRanksIfNeededTx(ctx, tx, newRank)
	})
}

// RankSet establishes absolute order across the given IDs by stacking them at
// the top of the rank space in the order supplied: ids[0] becomes topmost,
// ids[1] ranks just below, etc. Atomic — every assignment commits together
// or none does. Validates IDs exist and rejects duplicates before any write.
// [LAW:single-enforcer] Multi-issue rank reassignment lives in this one
// transaction so partial-application states cannot occur.
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

func (s *Store) RankSet(ctx context.Context, ids []string) error {
	if err := rankSetValidateIDs(ids); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.GetIssue(ctx, id); err != nil {
			return err
		}
	}
	return s.withMutation(ctx, "rank set", func(ctx context.Context, tx *sql.Tx) error {
		// Find the current topmost rank, excluding any of the IDs being reassigned
		// (so we anchor against rows that aren't moving).
		excludeIDs := make([]any, 0, len(ids))
		placeholders := make([]string, 0, len(ids))
		for _, id := range ids {
			excludeIDs = append(excludeIDs, id)
			placeholders = append(placeholders, "?")
		}
		query := fmt.Sprintf(`SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' AND id NOT IN (%s) ORDER BY item_rank ASC LIMIT 1`, strings.Join(placeholders, ","))
		var topRank sql.NullString
		if err := tx.QueryRowContext(ctx, query, excludeIDs...).Scan(&topRank); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("rank-set: query top: %w", err)
		}

		// Walk IDs in reverse, assigning each a rank just above the previous one.
		// The last ID (idx N-1) is anchored just above the existing top; each
		// earlier ID is anchored just above the previously-assigned rank, so the
		// final order is ids[0] < ids[1] < ... < ids[N-1] < (existing top).
		now := time.Now().UTC().Format(time.RFC3339Nano)
		cursor := topRank.String
		hasCursor := topRank.Valid && topRank.String != ""
		newRanks := make([]string, len(ids))
		for i := len(ids) - 1; i >= 0; i-- {
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
		for i, id := range ids {
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

// RankToBottom moves an issue to rank below all other issues.
func (s *Store) RankToBottom(ctx context.Context, issueID string) error {
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return err
	}
	return s.withMutation(ctx, "rank to bottom", func(ctx context.Context, tx *sql.Tx) error {
		var lastRank sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank != '' AND id != ? ORDER BY item_rank DESC LIMIT 1", issueID).Scan(&lastRank)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("rank-to-bottom: query last: %w", err)
		}
		var newRank string
		if !lastRank.Valid || lastRank.String == "" {
			newRank = rank.Initial()
		} else {
			newRank = rank.After(lastRank.String)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, issueID); err != nil {
			return fmt.Errorf("rank-to-bottom: update: %w", err)
		}
		return smoothRanksIfNeededTx(ctx, tx, newRank)
	})
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

// resolveComparableFrame maps a relative rank request onto the pair it is
// actually about. Rank meaning is frame-local: an issue's rank is only ever
// compared against its frame-mates (siblings under the same container, or
// fellow top-level items), so a request naming two issues from different
// frames is resolved to their representatives directly under the lowest
// common ancestor — ranking a standalone ticket against an epic's child
// behaves as ranking against the epic itself, and ranking the child against
// the standalone moves the epic. Nothing inside any epic is reordered.
// [LAW:types-are-the-program] Cross-frame midpoints are an illegal state of
// the rank keyspace; this resolution makes every write frame-coherent.
// Ranking an issue relative to its own container (or descendant) has no
// frame-coherent meaning and is rejected. [LAW:no-silent-failure]
func resolveComparableFrame(issueChain, targetChain []string) (movedID, anchorID string, err error) {
	pos := make(map[string]int, len(targetChain))
	for i, id := range targetChain {
		pos[id] = i
	}
	for i, id := range issueChain {
		j, ok := pos[id]
		if !ok {
			continue
		}
		if i == 0 {
			return "", "", fmt.Errorf("cannot rank %s relative to %s: %s contains it; rank it against a sibling instead", issueChain[0], targetChain[0], issueChain[0])
		}
		if j == 0 {
			return "", "", fmt.Errorf("cannot rank %s relative to %s: %s is inside %s; rank it against a sibling instead", issueChain[0], targetChain[0], issueChain[0], targetChain[0])
		}
		return issueChain[i-1], targetChain[j-1], nil
	}
	// Disjoint ancestries: the comparable frame is the top level.
	return issueChain[len(issueChain)-1], targetChain[len(targetChain)-1], nil
}

// resolveRankPair validates a relative rank request and resolves it to the
// frame-comparable pair, returning the hydrated anchor (its rank seeds the
// midpoint math) and the move record.
// [LAW:single-enforcer] Both relative rank ops route through this one
// resolution so cross-frame semantics cannot drift between above and below.
func (s *Store) resolveRankPair(ctx context.Context, issueID, targetID string) (model.Issue, RankMove, error) {
	if issueID == targetID {
		return model.Issue{}, RankMove{}, errors.New("cannot rank an issue relative to itself")
	}
	if _, err := s.GetIssue(ctx, targetID); err != nil {
		return model.Issue{}, RankMove{}, err
	}
	if _, err := s.GetIssue(ctx, issueID); err != nil {
		return model.Issue{}, RankMove{}, err
	}
	issueChain, err := s.ancestorChain(ctx, issueID)
	if err != nil {
		return model.Issue{}, RankMove{}, err
	}
	targetChain, err := s.ancestorChain(ctx, targetID)
	if err != nil {
		return model.Issue{}, RankMove{}, err
	}
	movedID, anchorID, err := resolveComparableFrame(issueChain, targetChain)
	if err != nil {
		return model.Issue{}, RankMove{}, err
	}
	anchor, err := s.GetIssue(ctx, anchorID)
	if err != nil {
		return model.Issue{}, RankMove{}, err
	}
	return anchor, RankMove{MovedID: movedID, AnchorID: anchorID}, nil
}

// RankAbove moves an issue to rank immediately above the target issue,
// after resolving both to their comparable frame (see resolveComparableFrame).
func (s *Store) RankAbove(ctx context.Context, issueID, targetID string) (RankMove, error) {
	target, move, err := s.resolveRankPair(ctx, issueID, targetID)
	if err != nil {
		return RankMove{}, err
	}
	return move, s.withMutation(ctx, "rank above", func(ctx context.Context, tx *sql.Tx) error {
		var aboveRank sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE item_rank < ? AND deleted_at IS NULL AND id != ? ORDER BY item_rank DESC LIMIT 1", target.Rank, move.MovedID).Scan(&aboveRank)
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
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, move.MovedID); err != nil {
			return fmt.Errorf("rank-above: update: %w", err)
		}
		return smoothRanksIfNeededTx(ctx, tx, newRank)
	})
}

// RankBelow moves an issue to rank immediately below the target issue,
// after resolving both to their comparable frame (see resolveComparableFrame).
func (s *Store) RankBelow(ctx context.Context, issueID, targetID string) (RankMove, error) {
	target, move, err := s.resolveRankPair(ctx, issueID, targetID)
	if err != nil {
		return RankMove{}, err
	}
	return move, s.withMutation(ctx, "rank below", func(ctx context.Context, tx *sql.Tx) error {
		var belowRank sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE item_rank > ? AND deleted_at IS NULL AND id != ? ORDER BY item_rank ASC LIMIT 1", target.Rank, move.MovedID).Scan(&belowRank)
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
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, move.MovedID); err != nil {
			return fmt.Errorf("rank-below: update: %w", err)
		}
		return smoothRanksIfNeededTx(ctx, tx, newRank)
	})
}

// smoothRanksIfNeeded checks whether the given rank string has grown past the
// smoothing threshold and, if so, re-spaces a local window of items around the
// insertion point. This keeps rank strings short with O(SmoothingWindow) cost
// instead of a full O(n) rebalance.
func smoothRanksIfNeededTx(ctx context.Context, tx *sql.Tx, triggerRank string) error {
	if len(triggerRank) < rank.SmoothingThreshold {
		return nil
	}
	half := rank.SmoothingWindow / 2

	// Collect the window: up to half items at or below the trigger, plus
	// up to half items above it.
	type ranked struct {
		id   string
		rank string
	}
	var window []ranked

	belowRows, err := tx.QueryContext(ctx,
		`SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank <= ? ORDER BY item_rank DESC LIMIT ?`,
		triggerRank, half)
	if err != nil {
		return fmt.Errorf("smooth: query below: %w", err)
	}
	var below []ranked
	for belowRows.Next() {
		var r ranked
		if err := belowRows.Scan(&r.id, &r.rank); err != nil {
			belowRows.Close()
			return fmt.Errorf("smooth: scan below: %w", err)
		}
		below = append(below, r)
	}
	belowRows.Close()
	if err := belowRows.Err(); err != nil {
		return fmt.Errorf("smooth: below rows: %w", err)
	}
	// Reverse below so it's in ascending order.
	for i, j := 0, len(below)-1; i < j; i, j = i+1, j-1 {
		below[i], below[j] = below[j], below[i]
	}
	window = append(window, below...)

	aboveRows, err := tx.QueryContext(ctx,
		`SELECT id, item_rank FROM issues WHERE deleted_at IS NULL AND item_rank > ? ORDER BY item_rank ASC LIMIT ?`,
		triggerRank, half)
	if err != nil {
		return fmt.Errorf("smooth: query above: %w", err)
	}
	for aboveRows.Next() {
		var r ranked
		if err := aboveRows.Scan(&r.id, &r.rank); err != nil {
			aboveRows.Close()
			return fmt.Errorf("smooth: scan above: %w", err)
		}
		window = append(window, r)
	}
	aboveRows.Close()
	if err := aboveRows.Err(); err != nil {
		return fmt.Errorf("smooth: above rows: %w", err)
	}

	if len(window) < 2 {
		return nil
	}

	// Find the boundary ranks just outside the window.
	var lowerBound, upperBound string
	loRow := tx.QueryRowContext(ctx,
		`SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank < ? ORDER BY item_rank DESC LIMIT 1`,
		window[0].rank)
	var lb sql.NullString
	if err := loRow.Scan(&lb); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("smooth: lower bound: %w", err)
	}
	if lb.Valid {
		lowerBound = lb.String
	}

	hiRow := tx.QueryRowContext(ctx,
		`SELECT item_rank FROM issues WHERE deleted_at IS NULL AND item_rank > ? ORDER BY item_rank ASC LIMIT 1`,
		window[len(window)-1].rank)
	var ub sql.NullString
	if err := hiRow.Scan(&ub); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("smooth: upper bound: %w", err)
	}
	if ub.Valid {
		upperBound = ub.String
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

// rankInversionCandidatesClause is a SQL pre-filter only: it picks blocks-edges
// where ranks are inverted and both endpoints are non-deleted. It deliberately
// does NOT filter on status — the canonical "is this issue closed?" predicate
// lives in the lifecycle (model.Issue.State()), not in the SQL row, because
// epics store status=NULL by design and their state is derived from children
// via AllOf (the issues_status_check constraint in migrations/00001_baseline.sql
// encodes this: epics have status IS NULL; leaves have status in the known
// set). A SQL-side
// `status != 'closed'` test evaluates to NULL (not TRUE) for every epic and
// would silently drop every blocks-edge that points at one. Liveness filtering
// is therefore done in Go after hydration — see Store.liveRankInversions.
// [LAW:one-source-of-truth] Lifecycle is the only authority for issue state;
// SQL paths that need that classification round-trip through model.State().
// [LAW:single-enforcer] Both Doctor count and FixRankInversions consume the
// same liveRankInversions helper so the two cannot drift in what they call
// an inversion.
const rankInversionCandidatesClause = `FROM relations r
	JOIN issues src ON src.id = r.src_id
	JOIN issues dst ON dst.id = r.dst_id
	WHERE r.type = 'blocks'
	AND src.deleted_at IS NULL AND dst.deleted_at IS NULL
	AND dst.item_rank > src.item_rank`

type rankInversion struct {
	depID       string // the dependency/blocker (should be ranked above)
	dependentID string // the dependent (src in blocks relation)
}

// rowQueryer abstracts the QueryContext surface that *sql.DB and *sql.Tx
// share, letting one helper run candidate-edge SQL inside or outside a tx.
// [LAW:single-enforcer] One inversion-loader for both Doctor (read, no tx)
// and FixRankInversions (mutating, intra-tx) — same SQL pre-filter, same
// liveness intersect, no second site.
type rowQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadInversionCandidates runs the SQL pre-filter and returns every blocks
// edge whose endpoints are non-deleted and whose dst is ranked below src.
// Liveness (is the issue closed per its lifecycle?) is filtered separately
// in Go — see filterLiveInversions.
func loadInversionCandidates(ctx context.Context, q rowQueryer) ([]rankInversion, error) {
	// In blocks relations: src_id is the dependent, dst_id is the dependency (blocker).
	rows, err := q.QueryContext(ctx, `SELECT r.dst_id, r.src_id `+rankInversionCandidatesClause+` ORDER BY src.item_rank ASC`)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()
	inversions := make([]rankInversion, 0)
	for rows.Next() {
		var inv rankInversion
		if err := rows.Scan(&inv.depID, &inv.dependentID); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		inversions = append(inversions, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return inversions, nil
}

// filterLiveInversions drops every candidate whose dependent or dependency
// has lifecycle State() == StateClosed. Closed work doesn't participate in
// dependency ordering, so an inversion across a closed endpoint is not an
// actionable inversion.
func filterLiveInversions(candidates []rankInversion, liveIDs map[string]struct{}) []rankInversion {
	out := make([]rankInversion, 0, len(candidates))
	for _, inv := range candidates {
		_, depLive := liveIDs[inv.depID]
		_, dependentLive := liveIDs[inv.dependentID]
		if depLive && dependentLive {
			out = append(out, inv)
		}
	}
	return out
}

// liveIssueIDs returns the set of non-archived, non-deleted issue IDs whose
// lifecycle State() is not Closed. Archived issues are user-deprioritized and
// do not generate actionable inversions, for the same reason closed issues do
// not. Lifecycle is computed via the canonical hydration path, so epics get
// AllOf rollup over their children — never a raw column peek that would lie
// about epic state.
// [LAW:one-source-of-truth] State classification rides the lifecycle here,
// the same predicate ready uses for its rank_inversion annotations.
func (s *Store) liveIssueIDs(ctx context.Context) (map[string]struct{}, error) {
	issues, err := s.ListIssues(ctx, ListIssuesFilter{Statuses: []model.State{model.StateOpen, model.StateInProgress}})
	if err != nil {
		return nil, fmt.Errorf("list live issues: %w", err)
	}
	out := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		out[issue.ID] = struct{}{}
	}
	return out, nil
}

// liveRankInversions returns blocks-edges whose dependency is ranked below
// the dependent and whose endpoints are both lifecycle-live. This is the
// single classification path; Doctor counts len(liveRankInversions) and
// FixRankInversions consumes the same inversions for remediation.
func (s *Store) liveRankInversions(ctx context.Context) ([]rankInversion, error) {
	liveIDs, err := s.liveIssueIDs(ctx)
	if err != nil {
		return nil, err
	}
	candidates, err := loadInversionCandidates(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("load inversion candidates: %w", err)
	}
	return filterLiveInversions(candidates, liveIDs), nil
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
// non-deleted. Unlike loadInversionCandidates it does not pre-filter on rank,
// because cycle detection asks about the constraint graph itself, not the
// current rank assignment.
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

// filterLiveBlocksEdges keeps only edges whose endpoints are both
// lifecycle-live, mirroring filterLiveInversions: a cycle through closed work
// cannot block the rank order of live work.
func filterLiveBlocksEdges(edges []blocksEdge, liveIDs map[string]struct{}) []blocksEdge {
	out := make([]blocksEdge, 0, len(edges))
	for _, e := range edges {
		_, depLive := liveIDs[e.dependency]
		_, dependentLive := liveIDs[e.dependent]
		if depLive && dependentLive {
			out = append(out, e)
		}
	}
	return out
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

// liveBlocksCycle returns the issue IDs forming a blocks dependency cycle among
// lifecycle-live issues, or nil if none. Doctor reports it and
// FixRankInversions refuses on it; both route through this one classifier so
// they cannot disagree about whether the store holds an unsatisfiable cycle.
// [LAW:single-enforcer]
func (s *Store) liveBlocksCycle(ctx context.Context) ([]string, error) {
	liveIDs, err := s.liveIssueIDs(ctx)
	if err != nil {
		return nil, err
	}
	edges, err := loadBlocksEdges(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("load blocks edges: %w", err)
	}
	return findBlocksCycle(filterLiveBlocksEdges(edges, liveIDs)), nil
}

// FixRankInversions finds all blocks relations where the dependency is ranked
// below the dependent and ranks each dependency above its dependent. Returns
// the number of dependency issues that were re-ranked.
func (s *Store) FixRankInversions(ctx context.Context) (int, error) {
	// Liveness is computed once before the tx: FixRankInversions only mutates
	// item_rank, so closure status is invariant across the loop's iterations.
	// Re-classifying inside the tx would require plumbing the queryer through
	// hydrateIssues; the snapshot semantics here are equivalent and simpler.
	// [LAW:dataflow-not-control-flow] Liveness is data the loop reads; it is
	// not branched on per-iteration.
	liveIDs, err := s.liveIssueIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("fix rank inversions: snapshot live set: %w", err)
	}
	loadInversions := func(ctx context.Context, tx *sql.Tx) ([]rankInversion, error) {
		candidates, err := loadInversionCandidates(ctx, tx)
		if err != nil {
			return nil, err
		}
		return filterLiveInversions(candidates, liveIDs), nil
	}
	serializeInversions := func(inversions []rankInversion) string {
		parts := make([]string, 0, len(inversions))
		for _, inv := range inversions {
			parts = append(parts, inv.depID+"<-"+inv.dependentID)
		}
		return strings.Join(parts, "|")
	}
	rerankedCount := 0
	if err := s.withMutation(ctx, "fix rank inversions", func(ctx context.Context, tx *sql.Tx) error {
		// A blocks cycle is unsatisfiable by any rank order: ranking each
		// dependency above its dependent would require placing every cycle
		// member above itself. Detect it before the rerank loop and fail with
		// the offending members, rather than oscillating between two equally
		// invalid states until the snapshot guard trips on an opaque message.
		// [LAW:types-are-the-program] The loop below assumes a DAG; this guard
		// is the constraint that makes that assumption true on entry.
		edges, err := loadBlocksEdges(ctx, tx)
		if err != nil {
			return fmt.Errorf("fix rank inversions: load blocks edges: %w", err)
		}
		if cycle := findBlocksCycle(filterLiveBlocksEdges(edges, liveIDs)); cycle != nil {
			return fmt.Errorf("fix rank inversions: blocks dependency cycle %s — a cycle has no valid rank order; break it by removing one edge with 'lit dep rm'", strings.Join(cycle, " -> "))
		}
		seenSnapshots := map[string]struct{}{}
		for {
			inversions, err := loadInversions(ctx, tx)
			if err != nil {
				return fmt.Errorf("fix rank inversions: %w", err)
			}
			if len(inversions) == 0 {
				return nil
			}
			snapshot := serializeInversions(inversions)
			if _, seen := seenSnapshots[snapshot]; seen {
				return fmt.Errorf("fix rank inversions: unable to converge in one run; remaining inversions=%d", len(inversions))
			}
			seenSnapshots[snapshot] = struct{}{}

			// [LAW:dataflow-not-control-flow] Every pass applies one deterministic update per dependency;
			// selected target dependents come from ordered inversion data rather than branch-specific handling.
			targets := make([]rankInversion, 0, len(inversions))
			seenDeps := map[string]struct{}{}
			for _, inv := range inversions {
				if _, seen := seenDeps[inv.depID]; seen {
					continue
				}
				seenDeps[inv.depID] = struct{}{}
				targets = append(targets, inv)
			}
			for _, target := range targets {
				// Place the dependency just above the highest-priority dependent by
				// computing a rank between the dependent's predecessor and the dependent itself.
				var targetRank string
				if err := tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE id = ?", target.dependentID).Scan(&targetRank); err != nil {
					return fmt.Errorf("fix rank inversions: read target rank %s: %w", target.dependentID, err)
				}
				var aboveRank sql.NullString
				err := tx.QueryRowContext(ctx, "SELECT item_rank FROM issues WHERE item_rank < ? AND deleted_at IS NULL AND id != ? ORDER BY item_rank DESC LIMIT 1", targetRank, target.depID).Scan(&aboveRank)
				if err != nil {
					if !errors.Is(err, sql.ErrNoRows) {
						return fmt.Errorf("fix rank inversions: query neighbor: %w", err)
					}
				}
				var newRank string
				if !aboveRank.Valid || aboveRank.String == "" {
					newRank = rank.Before(targetRank)
				} else {
					newRank, err = rank.Midpoint(aboveRank.String, targetRank)
					if err != nil {
						return fmt.Errorf("fix rank inversions: midpoint: %w", err)
					}
				}
				now := time.Now().UTC().Format(time.RFC3339Nano)
				if _, err := tx.ExecContext(ctx, "UPDATE issues SET item_rank = ?, updated_at = ? WHERE id = ?", newRank, now, target.depID); err != nil {
					return fmt.Errorf("fix rank inversions: update %s: %w", target.depID, err)
				}
				if err := smoothRanksIfNeededTx(ctx, tx, newRank); err != nil {
					return fmt.Errorf("fix rank inversions: smooth ranks: %w", err)
				}
				rerankedCount++
			}
		}
	}); err != nil {
		return 0, err
	}
	return rerankedCount, nil
}
