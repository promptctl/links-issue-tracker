package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Undo reads a write back out of the store's own history and reverts it. A
// write is one Dolt commit on the data branch; what it changed is the store on
// both sides of it, read as of its parent and as of the commit. Nothing is
// recorded at write time and no branch moves: every read here is AS OF a
// commit, so listing writes takes no lock and works on a read-only open.
// storage.Classify and storage.Revert decide; this file only reads, and lands
// the result through bulkApply, the one bulk-edit path. [LAW:effects-at-boundaries]

// revision is where a read looks: the store as it stands, or as it stood at a
// commit. One reader per slot takes a revision, so the store now and both
// sides of a write are read the same way. [LAW:one-source-of-truth]
type revision struct {
	asOf string
}

// liveRevision is the store as it stands.
var liveRevision = revision{}

// atCommit is the store as it stood at commit. AS OF takes a literal, not a
// bound parameter, so the hash is interpolated and must be a Dolt hash.
func atCommit(commit string) (revision, error) {
	if !isDoltCommitHash(commit) {
		return revision{}, fmt.Errorf("read as of commit: %q is not a Dolt commit hash", commit)
	}
	return revision{asOf: commit}, nil
}

// table is the reference to name in a FROM clause to read it at r.
func (r revision) table(name string) string {
	if r.asOf == "" {
		return name
	}
	return fmt.Sprintf("%s AS OF '%s'", name, r.asOf)
}

// issueFieldsAt reads each of ids at r as its row's issue with no lifecycle
// (rowFields), beside its row's lifecycle columns. An id absent at r is absent
// from both maps. A rank write re-keys thousands of ids, so they are an idSet.
func (s *Store) issueFieldsAt(ctx context.Context, r revision, ids []string) (map[string]model.Issue, map[string]model.StatusView, error) {
	issues := map[string]model.Issue{}
	statuses := map[string]model.StatusView{}
	named, arg := newIDSet(ids).rows("named")
	rows, err := s.scanIssueRows(ctx, `SELECT `+issueColumnsQualified+` FROM `+named+` JOIN `+r.table("issues")+` i ON i.id = named.id`, []any{arg})
	if err != nil {
		return nil, nil, err
	}
	labels := map[string][]string{}
	if err := s.queryRows(ctx, `SELECT l.issue_id, l.label FROM `+named+` JOIN `+r.table("labels")+` l ON l.issue_id = named.id ORDER BY l.label`, []any{arg}, func(rows *sql.Rows) error {
		var issue, label string
		if err := rows.Scan(&issue, &label); err != nil {
			return err
		}
		labels[issue] = append(labels[issue], label)
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("read labels: %w", err)
	}
	for _, row := range rows {
		issues[row.Issue.ID] = rowFields(row.Issue, labels[row.Issue.ID])
		statuses[row.Issue.ID] = row.Status
	}
	return issues, statuses, nil
}

// edgeTargetsAt reads, for each of srcs, the issues its edges of relType point
// at, at r.
func (s *Store) edgeTargetsAt(ctx context.Context, r revision, relType model.RelationType, srcs []string) (map[string][]string, error) {
	targets := map[string][]string{}
	named, arg := newIDSet(srcs).rows("named")
	err := s.queryRows(ctx, `SELECT e.src_id, e.dst_id FROM `+named+` JOIN `+r.table("relations")+` e ON e.src_id = named.id WHERE e.type = ? ORDER BY e.dst_id`, []any{arg, string(relType)}, func(rows *sql.Rows) error {
		var src, dst string
		if err := rows.Scan(&src, &dst); err != nil {
			return err
		}
		targets[src] = append(targets[src], dst)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read %s edges: %w", relType, err)
	}
	return targets, nil
}

// queryRows runs a read and hands each row to scan.
func (s *Store) queryRows(ctx context.Context, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// write is one commit of the data branch's history, as the log names it.
type write struct {
	id      string
	at      time.Time
	message string
	order   int64
}

// writeLogColumns is a write as dolt_log reads it.
const writeLogColumns = `commit_hash, date, message, commit_order`

func scanWrite(rows *sql.Rows) (write, error) {
	var w write
	err := rows.Scan(&w.id, &w.at, &w.message, &w.order)
	return w, err
}

// recentWrites is the newest last writes of the data branch, newest first.
func (s *Store) recentWrites(ctx context.Context, last int) ([]write, error) {
	var writes []write
	err := s.queryRows(ctx, `SELECT `+writeLogColumns+` FROM dolt_log LIMIT ?`, []any{last}, func(rows *sql.Rows) error {
		w, err := scanWrite(rows)
		writes = append(writes, w)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read the newest writes: %w", err)
	}
	return writes, nil
}

// namedWrites resolves ids to writes on the data branch's history, newest
// first, each once. An id the history does not hold is refused: history past
// the window has been cut away, and a sync replay gives local writes new ids.
func (s *Store) namedWrites(ctx context.Context, ids []string) ([]write, error) {
	found := map[string]write{}
	named, arg := newIDSet(ids).rows("named")
	err := s.queryRows(ctx, `SELECT `+writeLogColumns+` FROM `+named+` JOIN dolt_log ON commit_hash = named.id`, []any{arg}, func(rows *sql.Rows) error {
		w, err := scanWrite(rows)
		found[w.id] = w
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read the named writes: %w", err)
	}
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			return nil, fmt.Errorf("%w: history past the kept window is gone, and a sync replay gives local writes new ids; `lit undo list` shows the current ids", storage.NotFoundError{Entity: "write", ID: id})
		}
	}
	writes := slices.Collect(maps.Values(found))
	// commit_order ties across the branches of a merged history; the commit
	// date, then the id, break the tie, so a fold of the same writes runs
	// newest first, in the same order every time.
	slices.SortFunc(writes, func(a, b write) int {
		return cmp.Or(cmp.Compare(b.order, a.order), b.at.Compare(a.at), strings.Compare(a.id, b.id))
	})
	return writes, nil
}

// parentsOf is a commit's parents, first parent first. Dolt records the
// store's first commit with one NULL parent, which is no parent.
func (s *Store) parentsOf(ctx context.Context, commit string) ([]string, error) {
	var parents []string
	err := s.queryRows(ctx, `SELECT parent_hash FROM dolt_commit_ancestors WHERE commit_hash = ? ORDER BY parent_index`, []any{commit}, func(rows *sql.Rows) error {
		var parent sql.NullString
		if err := rows.Scan(&parent); err != nil {
			return err
		}
		if parent.Valid {
			parents = append(parents, parent.String)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the parents of %s: %w", commit, err)
	}
	return parents, nil
}

// changedTables is every table w's commit changed, by name.
func (s *Store) changedTables(ctx context.Context, before, after revision) ([]string, error) {
	var tables []string
	err := s.queryRows(ctx, fmt.Sprintf(`SELECT COALESCE(to_table_name, from_table_name) FROM dolt_diff_summary('%s', '%s')`, before.asOf, after.asOf), nil, func(rows *sql.Rows) error {
		var table string
		err := rows.Scan(&table)
		tables = append(tables, table)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read the tables the write changed: %w", err)
	}
	slices.Sort(tables)
	return tables, nil
}

// diffKey is one changed row of a table, by its key on each side: from is
// empty for a row the write added, to for one it removed.
type diffKey struct {
	from, to []string
}

func (k diffKey) added() bool   { return k.from == nil }
func (k diffKey) removed() bool { return k.to == nil }

// changedKeys reads the keys of the rows of table a write changed, keyed by
// cols on each side.
func (s *Store) changedKeys(ctx context.Context, before, after revision, table string, cols ...string) ([]diffKey, error) {
	selected := make([]string, 0, 2*len(cols))
	for _, side := range []string{"from_", "to_"} {
		for _, col := range cols {
			selected = append(selected, side+col)
		}
	}
	var keys []diffKey
	err := s.queryRows(ctx, fmt.Sprintf(`SELECT %s FROM dolt_diff('%s', '%s', '%s')`, strings.Join(selected, ", "), before.asOf, after.asOf, table), nil, func(rows *sql.Rows) error {
		values := make([]sql.NullString, len(selected))
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		side := func(values []sql.NullString) []string {
			if !values[0].Valid {
				return nil
			}
			out := make([]string, len(values))
			for i, v := range values {
				out[i] = v.String
			}
			return out
		}
		keys = append(keys, diffKey{from: side(values[:len(cols)]), to: side(values[len(cols):])})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the %s rows the write changed: %w", table, err)
	}
	return keys, nil
}

// writeRows reads what w changed: the store on both sides of it, as
// storage.Classify takes it. headSchema is the schema the store is at now; a
// side at another one is read in a shape this binary does not, so the write is
// unrevertable and nothing of it is read.
func (s *Store) writeRows(ctx context.Context, w write, headSchema int64) (storage.WriteRows, error) {
	rows := storage.WriteRows{Write: w.id}
	outside := func(what, reverse string) {
		rows.Outside = append(rows.Outside, storage.Unrevertable{Write: w.id, What: what, Reverse: reverse})
	}
	parents, err := s.parentsOf(ctx, w.id)
	if err != nil {
		return rows, err
	}
	if len(parents) != 1 {
		outside(fmt.Sprintf("a commit with %d parents: the store's first commit, or a merge of two histories", len(parents)), "")
		return rows, nil
	}
	before, err := atCommit(parents[0])
	if err != nil {
		return rows, err
	}
	after, err := atCommit(w.id)
	if err != nil {
		return rows, err
	}
	versions := make([]int64, 2)
	for i, side := range []revision{before, after} {
		if versions[i], err = s.schemaVersionAtCommit(ctx, side.asOf); err != nil {
			return rows, err
		}
	}
	if versions[0] != headSchema || versions[1] != headSchema {
		outside(fmt.Sprintf("written across schema versions (v%d before it, v%d after; the store is at v%d)", versions[0], versions[1], headSchema), "")
		return rows, nil
	}
	tables, err := s.changedTables(ctx, before, after)
	if err != nil {
		return rows, err
	}
	// made is every issue the write created or removed. Its labels, edges, and
	// events are that making or removal, reported once where its row is read,
	// not moves of their own.
	made := map[string]bool{}
	var fieldIDs, children []string
	var edges, events, changes []diffKey
	for _, table := range tables {
		switch table {
		case "issues":
			keys, err := s.changedKeys(ctx, before, after, table, "id")
			if err != nil {
				return rows, err
			}
			for _, key := range keys {
				switch {
				case key.added():
					outside(fmt.Sprintf("%s created", key.to[0]), "`lit delete` removes a ticket")
					made[key.to[0]] = true
				case key.removed():
					outside(fmt.Sprintf("%s removed", key.from[0]), "")
					made[key.from[0]] = true
				default:
					fieldIDs = append(fieldIDs, key.to[0])
				}
			}
		case "labels":
			keys, err := s.changedKeys(ctx, before, after, table, "issue_id")
			if err != nil {
				return rows, err
			}
			for _, key := range keys {
				fieldIDs = append(fieldIDs, slices.Concat(key.from, key.to)[0])
			}
		case "relations":
			if edges, err = s.changedKeys(ctx, before, after, table, "src_id", "dst_id", "type"); err != nil {
				return rows, err
			}
		case "issue_events":
			if events, err = s.changedKeys(ctx, before, after, table, "id", "issue_id"); err != nil {
				return rows, err
			}
		case "issue_event_changes":
			if changes, err = s.changedKeys(ctx, before, after, table, "event_id", "field"); err != nil {
				return rows, err
			}
		default:
			outside(fmt.Sprintf("the %s table changed", table), "")
		}
	}
	fieldIDs = slices.DeleteFunc(fieldIDs, func(issue string) bool { return made[issue] })
	for _, key := range edges {
		edge := slices.Concat(key.from, key.to)
		src, dst, relType := edge[0], edge[1], model.RelationType(edge[2])
		switch {
		case made[src] || made[dst]:
			// An edge of a ticket the write made or removed, at either end.
		case !key.added() && !key.removed():
			// A row on both sides changed only its created_at/created_by
			// stamps, which a re-set rewrites; it moved no edge.
		case relType == model.RelParentChild:
			children = append(children, src)
		case relType == model.RelBlockedBy:
			slot := storage.Slot{Kind: storage.BlockedBySlot, Issue: src, Name: dst}
			rows.BlockedBy = append(rows.BlockedBy, storage.Move{Slot: slot, From: storage.EdgeValue(key.removed()), To: storage.EdgeValue(key.added())})
		default:
			outside(fmt.Sprintf("%s %s %s edge", src, relType, dst), "`lit dep` adds and removes it")
		}
	}
	// A change row moves with its event's row: read with an event the write
	// added, or gone with one it removed. One whose event row the write left
	// alone rewrote that event's history.
	moved := map[string]bool{}
	var added []string
	for _, key := range events {
		event := slices.Concat(key.from, key.to)
		moved[event[0]] = true
		switch {
		case made[event[1]]:
			// Its issue's making or removal, already reported.
		case key.added():
			added = append(added, event[0])
		default:
			outside(fmt.Sprintf("history event %s rewritten or removed", event[0]), "")
		}
	}
	for _, key := range changes {
		if change := slices.Concat(key.from, key.to); !moved[change[0]] {
			outside(fmt.Sprintf("history event %s: its %s change rewritten", change[0], change[1]), "")
		}
	}
	if rows.Records, err = s.eventsAt(ctx, after, added); err != nil {
		return rows, err
	}
	if err := s.readFieldMoves(ctx, before, after, uniqueSorted(fieldIDs), &rows, outside); err != nil {
		return rows, err
	}
	if err := s.readParentMoves(ctx, before, after, uniqueSorted(children), &rows); err != nil {
		return rows, err
	}
	return rows, nil
}

// readFieldMoves reads ids on both sides of a write: each editable field whose
// value differs is a move, and anything else of the row that differs, other
// than updated_at, is outside what a document can put back.
func (s *Store) readFieldMoves(ctx context.Context, before, after revision, ids []string, rows *storage.WriteRows, outside func(what, reverse string)) error {
	was, wasStatus, err := s.issueFieldsAt(ctx, before, ids)
	if err != nil {
		return err
	}
	is, isStatus, err := s.issueFieldsAt(ctx, after, ids)
	if err != nil {
		return err
	}
	var ranked int
	for _, id := range ids {
		from, to := storage.IssueFieldValues(was[id]), storage.IssueFieldValues(is[id])
		for _, name := range storage.IssueFieldNames() {
			if from[name] != to[name] {
				slot := storage.Slot{Kind: storage.FieldSlot, Issue: id, Name: name}
				rows.Fields = append(rows.Fields, storage.Move{Slot: slot, From: storage.FieldValue(from[name]), To: storage.FieldValue(to[name])})
			}
		}
		// What is left different once both sides carry the newer side's
		// fields is what moved outside the editable set. Both go through the
		// same rows, so a value the rows normalize reads alike on both sides.
		// [LAW:one-source-of-truth] the editable set is the field table's.
		carried, wasErr := storage.WithIssueFieldsOf(was[id], is[id])
		self, isErr := storage.WithIssueFieldsOf(is[id], is[id])
		if err := cmp.Or(wasErr, isErr); err != nil {
			outside(fmt.Sprintf("%s: %v", id, err), "")
			continue
		}
		carried.UpdatedAt = self.UpdatedAt
		if !reflect.DeepEqual(wasStatus[id], isStatus[id]) {
			outside(fmt.Sprintf("%s: its lifecycle moved", id), "lifecycle moves only through its verbs")
		}
		if carried.Rank != self.Rank {
			ranked++
			carried.Rank = self.Rank
		}
		if !reflect.DeepEqual(carried, self) {
			outside(fmt.Sprintf("%s: its row moved outside its editable fields", id), "")
		}
	}
	// One rank write re-keys every ticket it spaces around the one it places,
	// so its moves are one fact, counted, not a line per re-keyed ticket.
	if ranked > 0 {
		outside(fmt.Sprintf("ranks moved, tickets re-keyed: %d", ranked), "`lit rank` places a ticket")
	}
	return nil
}

// readParentMoves reads each child's parent set on both sides of a write.
func (s *Store) readParentMoves(ctx context.Context, before, after revision, children []string, rows *storage.WriteRows) error {
	was, err := s.edgeTargetsAt(ctx, before, model.RelParentChild, children)
	if err != nil {
		return err
	}
	is, err := s.edgeTargetsAt(ctx, after, model.RelParentChild, children)
	if err != nil {
		return err
	}
	for _, child := range children {
		from, to := storage.ParentsValue(was[child]), storage.ParentsValue(is[child])
		if from != to {
			rows.Parents = append(rows.Parents, storage.Move{Slot: storage.Slot{Kind: storage.ParentSlot, Issue: child}, From: from, To: to})
		}
	}
	return nil
}

// eventsAt reads the events ids at r with their change rows, through
// queryEvents, the one reader of events.
func (s *Store) eventsAt(ctx context.Context, r revision, ids []string) ([]model.IssueEvent, error) {
	named, arg := newIDSet(ids).rows("named")
	selection := `SELECT ` + eventColumns + `, c.field, c.from_value, c.to_value FROM ` + named + ` JOIN ` + r.table("issue_events") + ` e ON e.id = named.id LEFT JOIN ` + r.table("issue_event_changes") + ` c ON c.event_id = e.id`
	events, err := s.queryEvents(ctx, selection, arg)
	if err != nil {
		return nil, fmt.Errorf("read the events the write added: %w", err)
	}
	return events, nil
}

func uniqueSorted(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}

// headSchema is the schema the store is at now.
func (s *Store) headSchema(ctx context.Context) (int64, error) {
	head, err := readDoltHead(ctx, s.db)
	if err != nil {
		return 0, err
	}
	return s.schemaVersionAtCommit(ctx, head)
}

// ListWrites is the newest last writes, newest first, each with what it moved
// and what of it no document can put back.
func (s *Store) ListWrites(ctx context.Context, last int) ([]storage.WriteSummary, error) {
	schema, err := s.headSchema(ctx)
	if err != nil {
		return nil, err
	}
	writes, err := s.recentWrites(ctx, last)
	if err != nil {
		return nil, err
	}
	summaries := make([]storage.WriteSummary, 0, len(writes))
	for _, w := range writes {
		rows, err := s.writeRows(ctx, w, schema)
		if err != nil {
			return nil, fmt.Errorf("read write %s: %w", w.id, err)
		}
		_, unrevertable := storage.Classify(rows)
		summaries = append(summaries, storage.WriteSummary{
			ID:           w.id,
			At:           w.at,
			Message:      w.message,
			Moves:        slices.Concat(rows.Fields, rows.Parents, rows.BlockedBy),
			Unrevertable: unrevertable,
		})
	}
	return summaries, nil
}

// RevertWrites puts back what the writes ids moved, as one bulk edit through
// bulkApply. Every write is read and classified before the commit lock is
// taken; under it, the store as it stands is read, the writes are folded onto
// it, and the bulk edit lands, so the conflict check and the landing see one
// state. Any unrevertable write or conflict refuses the whole undo and writes
// nothing.
func (s *Store) RevertWrites(ctx context.Context, ids []string, actor string) (storage.RevertResult, error) {
	schema, err := s.headSchema(ctx)
	if err != nil {
		return storage.RevertResult{}, err
	}
	writes, err := s.namedWrites(ctx, ids)
	if err != nil {
		return storage.RevertResult{}, err
	}
	deltas := make([]storage.WriteDelta, 0, len(writes))
	var refused []storage.Unrevertable
	for _, w := range writes {
		rows, err := s.writeRows(ctx, w, schema)
		if err != nil {
			return storage.RevertResult{}, fmt.Errorf("read write %s: %w", w.id, err)
		}
		delta, unrevertable := storage.Classify(rows)
		refused = append(refused, unrevertable...)
		deltas = append(deltas, delta)
	}
	if len(refused) > 0 {
		return storage.RevertResult{}, storage.RevertRefusedError{Unrevertable: refused}
	}
	reverting := make([]string, len(writes))
	for i, w := range writes {
		reverting[i] = w.id
	}
	var result storage.RevertResult
	// The undo is a batch around the bulk edit's batch: the observers hear
	// what stands once this hold is released, not inside it.
	var heard []heardChange
	err = s.withCommitLock(ctx, func(ctx context.Context) error {
		ctx = context.WithValue(ctx, batchHearingKey{}, &heard)
		now, err := s.currentState(ctx, deltas)
		if err != nil {
			return err
		}
		reverted, conflicts := storage.Revert(deltas, now)
		if len(conflicts) > 0 {
			return storage.RevertConflictError{Conflicts: conflicts}
		}
		if len(reverted.Documents) == 0 {
			return storage.RevertHeldError{Writes: reverting}
		}
		// What the bulk edit left written stands beside any error it returns
		// (withBatch), so the undo reports it on every outcome. A refusal
		// before its batch began names no revert point and wrote nothing.
		// [LAW:nothing-unseen]
		applied, from, applyErr := s.bulkApply(ctx, "", actor, reverted.Documents)
		if from == "" {
			return applyErr
		}
		result.Applied = applied
		landed, landedErr := s.commitsAbove(ctx, from)
		result.Landed = landed
		if err := errors.Join(applyErr, landedErr); err != nil {
			return err
		}
		result.Writes, result.Moves = reverting, reverted.Moves
		return nil
	})
	s.hear(ctx, heard...)
	return result, err
}

// currentState reads the store as it stands for exactly the slots deltas
// moved, through the same readers that read each side of a write. Every issue
// a slot names is read, so an issue a later write removed is known removed.
func (s *Store) currentState(ctx context.Context, deltas []storage.WriteDelta) (storage.CurrentState, error) {
	var named, children, blocked []string
	for _, delta := range deltas {
		for _, move := range delta.Moves() {
			named = append(named, move.Slot.Issue)
			switch move.Slot.Kind {
			case storage.ParentSlot:
				children = append(children, move.Slot.Issue)
			case storage.BlockedBySlot:
				blocked = append(blocked, move.Slot.Issue)
			}
		}
	}
	issues, _, err := s.issueFieldsAt(ctx, liveRevision, named)
	if err != nil {
		return storage.CurrentState{}, err
	}
	now := storage.CurrentState{Standing: map[string]bool{}, Fields: map[storage.Slot]storage.SlotValue{}}
	for id, issue := range issues {
		now.Standing[id] = true
		for name, value := range storage.IssueFieldValues(issue) {
			now.Fields[storage.Slot{Kind: storage.FieldSlot, Issue: id, Name: name}] = storage.FieldValue(value)
		}
	}
	if now.Parents, err = s.edgeTargetsAt(ctx, liveRevision, model.RelParentChild, children); err != nil {
		return storage.CurrentState{}, err
	}
	if now.BlockedBy, err = s.edgeTargetsAt(ctx, liveRevision, model.RelBlockedBy, blocked); err != nil {
		return storage.CurrentState{}, err
	}
	return now, nil
}

// commitsAbove is the commits the data branch holds above from, oldest first:
// what a batch whose revert point is from landed.
func (s *Store) commitsAbove(ctx context.Context, from string) ([]string, error) {
	var commits []string
	query, args := foldedChainQuery("commit_hash", "HEAD", from)
	err := s.queryRows(ctx, query, args, func(rows *sql.Rows) error {
		var commit string
		err := rows.Scan(&commit)
		commits = append(commits, commit)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read the commits the undo landed: %w", err)
	}
	slices.Reverse(commits)
	return commits, nil
}

var _ storage.Reverter = (*Store)(nil)
