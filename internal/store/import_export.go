package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func (s *Store) Export(ctx context.Context) (model.Export, error) {
	issues, err := s.ListIssues(ctx, storage.ListIssuesFilter{Limit: 0, IncludeArchived: true, IncludeDeleted: true})
	if err != nil {
		return model.Export{}, err
	}
	rels, err := s.listAllRelations(ctx)
	if err != nil {
		return model.Export{}, err
	}
	comments, err := s.listAllComments(ctx)
	if err != nil {
		return model.Export{}, err
	}
	labels, err := s.listAllLabels(ctx)
	if err != nil {
		return model.Export{}, err
	}
	events, err := s.ListAllEvents(ctx)
	if err != nil {
		return model.Export{}, err
	}
	purges, err := listPurges(ctx, s.db)
	if err != nil {
		return model.Export{}, err
	}
	// hydrateIssues guarantees every Issue it returns is fully hydrated
	// (post-condition in store.go), so Export does not re-check. Issue.MarshalJSON
	// remains the boundary that rejects partial values from any other source.
	return model.Export{Version: model.ExportVersion, WorkspaceID: s.workspaceID, ExportedAt: s.clock.Now(), Issues: issues, Relations: rels, Comments: comments, Labels: labels, Events: events, Purges: purges}, nil
}

func (s *Store) Doctor(ctx context.Context) (storage.HealthReport, error) {
	report := storage.HealthReport{
		DependencyCycle: []string{},
		ParentCycle:     []string{},
		Unchecked:       []string{},
		Errors:          []string{},
		Warnings:        []string{},
	}
	report.IntegrityCheck = "ok"
	var violations int
	if err := s.db.QueryRowContext(ctx, `CALL DOLT_VERIFY_CONSTRAINTS()`).Scan(&violations); err != nil {
		return report, fmt.Errorf("verify constraints: %w", err)
	}
	if violations > 0 {
		report.IntegrityCheck = "constraint_violations"
		report.Errors = append(report.Errors, fmt.Sprintf("constraint violations: %d", violations))
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM relations r LEFT JOIN issues s ON s.id = r.src_id LEFT JOIN issues d ON d.id = r.dst_id WHERE s.id IS NULL OR d.id IS NULL`,
		`SELECT COUNT(*) FROM comments c LEFT JOIN issues i ON i.id = c.issue_id WHERE i.id IS NULL`,
		`SELECT COUNT(*) FROM labels l LEFT JOIN issues i ON i.id = l.issue_id WHERE i.id IS NULL`,
	} {
		var count int
		if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return report, fmt.Errorf("count foreign key issues: %w", err)
		}
		report.ForeignKeyIssues += count
	}
	if report.ForeignKeyIssues > 0 {
		report.Errors = append(report.Errors, fmt.Sprintf("foreign key violations: %d", report.ForeignKeyIssues))
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM relations WHERE type='related-to' AND src_id >= dst_id`).Scan(&report.InvalidRelatedRows); err != nil {
		return report, fmt.Errorf("count invalid related rows: %w", err)
	}
	if report.InvalidRelatedRows > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("invalid related-to ordering rows: %d", report.InvalidRelatedRows))
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_events e LEFT JOIN issues i ON i.id = e.issue_id WHERE i.id IS NULL`).Scan(&report.OrphanHistoryRows); err != nil {
		return report, fmt.Errorf("count orphan event rows: %w", err)
	}
	if report.OrphanHistoryRows > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("orphan issue event rows: %d", report.OrphanHistoryRows))
	}
	// Containment is refused at every write, and a sync or a restore homes
	// what it lands (storage.HomeStranded), so a ticket outside an epic is what
	// neither was there to refuse: a recovery rebuild writes its source as it
	// was (rebuildFromExport), and a pre-v7 source holds them. It is a column
	// read, no hydration, so it runs on a hierarchy holding a loop. A warning,
	// not an error: every read still answers over it, and the faithful rebuild
	// reproduces it, which the recovery gate must not refuse (healthFindings).
	// [LAW:single-enforcer]
	containment, err := containmentFindings(ctx, s.db)
	if err != nil {
		return report, fmt.Errorf("containment check: %w", err)
	}
	report.Warnings = append(report.Warnings, containment...)
	// ORDERING IS LOAD-BEARING: this check must precede every hydrating read in
	// Doctor, and the rank and dependency checks below are hydrating reads. Do
	// not move it down for readability and do not hoist a shared listing above
	// it — a Go stack overflow is not recoverable, so the result is not a worse
	// report but no report at all, from the one command that can explain why the
	// workspace is unreadable. TestDoctorNamesAStoredParentCycle is what catches
	// it: with this block moved below, that test dies by stack overflow.
	//
	// A parent cycle is checked before everything below it, because it is the
	// one finding that makes the rest unobtainable rather than merely worse. A
	// hierarchy with a loop has no root, so the walks built on it run forever.
	// The liveness classification the rank checks start from is s.ListIssues,
	// which hydrates, and hydration derives container state by descending into
	// each epic's children — so on a loop it recurses between the loop's members
	// and overflows the stack rather than returning a wrong number. (The walk
	// that kills it descends; looking only for an ancestor walk misses it.) Reading the edges straight from the relations table is the only
	// question that can still be answered on that data, so it is asked first
	// and answered alone.
	//
	// The write boundary refuses the edge that closes a loop (rejectParentCycle),
	// so this reports only what was already stored: data written before that
	// rule, or restored from an export. Refuse at the boundary, report what the
	// boundary was not there to refuse — the same division the blocks cycle
	// already uses. [LAW:single-enforcer]
	parentOf, err := loadParentEdges(ctx, s.db)
	if err != nil {
		return report, fmt.Errorf("parent cycle check: %w", err)
	}
	if cycle := parentCycle(parentOf); len(cycle) > 0 {
		report.ParentCycle = cycle
		// The checks below never run, and the report says so rather than leaving
		// their zero values to be read as findings of nothing.
		// [LAW:no-silent-failure]
		report.Unchecked = []string{storage.CheckRankInversions, storage.CheckDuplicateRanks, storage.CheckDependencyCycle}
		// An error, not a warning: this is reported through the arm that makes
		// `lit doctor` exit nonzero, so the zeros left on the unrun checks below
		// can never be read as a clean bill of health. [LAW:no-silent-failure]
		report.Errors = append(report.Errors, fmt.Sprintf("parent cycle: %s (a hierarchy has no root once it loops; every walk up this chain runs forever, so the rank and dependency checks below could not be run — %s, then re-run)", strings.Join(cycle, " -> "), parentCycleRepair))
		return report, nil
	}
	// A child more than one framing edge claims still lists — under its lowest
	// parent id, which is where tree order and the frame lookup both put it —
	// so this is the one place the fault is said out loud, and the checks
	// below it still run. [LAW:no-silent-failure] [LAW:single-enforcer]
	ancestry, err := loadRankAncestry(ctx, s.db)
	if err != nil {
		return report, fmt.Errorf("rank ancestry: %w", err)
	}
	for _, conflict := range ancestry.Conflicts() {
		report.Errors = append(report.Errors, conflict.Finding())
	}
	// Rank inversions, duplicate keys and a blocked-by dependency cycle are
	// questions about one snapshot — the rank order and the blocked-by edges —
	// so it is read once.
	// Liveness is the lifecycle's classification, never a SQL filter; see
	// liveIssueIDs.
	liveIDs, err := s.liveIssueIDs(ctx)
	if err != nil {
		return report, fmt.Errorf("rank checks: %w", err)
	}
	order, err := loadRankOrder(ctx, s.db)
	if err != nil {
		return report, fmt.Errorf("rank checks: %w", err)
	}
	edges, err := loadBlocksEdges(ctx, s.db)
	if err != nil {
		return report, fmt.Errorf("rank checks: %w", err)
	}
	// [LAW:single-enforcer] This number and what `--fix` leaves behind read the
	// same live edges (store.liveEdges) through one storage.DependencyOrder, the
	// one definition of "inverted". TestRepairRankOrderProperties asserts the
	// join: the order the repair returns leaves invertedEdges empty, so this
	// count going to zero is a property of the repair rather than a second
	// opinion about it.
	live := liveEdges(edges, liveIDs)
	dependencies := storage.NewDependencyOrder(ancestry, live)
	report.RankInversions = len(invertedEdges(order, dependencies, live))
	if report.RankInversions > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("rank inversions: %d (dependencies ranked below dependents)", report.RankInversions))
	}
	// An interleaved plan is workable, so it is named rather than refused; the
	// warning says why a dependency it lists below its dependent is not counted
	// above. [LAW:nothing-unseen]
	report.Interleaved = dependencies.Interleaved()
	for _, group := range report.Interleaved {
		report.Warnings = append(report.Warnings, fmt.Sprintf("interleaved: %s — work in each waits on work in another, so no rank order lists every dependency among them first; 'lit doctor --fix=rank' leaves their order as it stands", strings.Join(group, ", ")))
	}
	// No rank verb places a key one placed issue holds onto another, and a
	// landing separates the keys it brings in, so this names what neither stood
	// in front of: data stored before either did. [LAW:single-enforcer]
	// rankCollisions is the one definition of a duplicate, and the rank repair
	// leaves none behind among the issues this order holds.
	duplicates := rankCollisions(order)
	report.DuplicateRanks = len(duplicates)
	if report.DuplicateRanks > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("duplicate rank keys: %d (%s; each key's holders list by id instead of by rank — 'lit doctor --fix=rank' gives each its own key without changing the order)", report.DuplicateRanks, describeRankCollisions(duplicates)))
	}
	// A blocked-by dependency cycle is work none of which can start, and it
	// refuses the rank repair as a whole. Surface the members so the operator
	// knows exactly which edge to remove.
	// [LAW:single-enforcer] blocksCycle is what FixRankInversions refuses on,
	// so this reports a cycle exactly when the repair refuses.
	if cycle := blocksCycle(order, live); len(cycle) > 0 {
		report.DependencyCycle = cycle
		report.Warnings = append(report.Warnings, fmt.Sprintf("blocked-by dependency cycle: %s (no rank order exists; remove one edge with 'lit dep rm' to break it)", strings.Join(cycle, " -> ")))
	}
	return report, nil
}

// FixIntegrity repairs what this engine can be structurally wrong about, then
// re-examines. The repair always runs — the caller that only wanted a look
// called Doctor. [LAW:dataflow-not-control-flow]
func (s *Store) FixIntegrity(ctx context.Context) (storage.HealthReport, error) {
	if err := s.withMutation(ctx, "fix integrity", func(ctx context.Context, tx *sql.Tx) error {
		// The `NOT IN` here carries a SUBQUERY, not a placeholder list, and
		// that is what keeps it off the quadratic path idBatchSize describes:
		// the engine plans one anti-join against a table, where a list of N
		// literals would become N index ranges to merge pairwise. It scales
		// with the backlog, and linearly.
		if _, err := tx.ExecContext(ctx, `DELETE FROM issue_events WHERE issue_id NOT IN (SELECT id FROM issues)`); err != nil {
			return fmt.Errorf("repair orphan events: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM relations WHERE type='related-to' AND src_id = dst_id`); err != nil {
			return fmt.Errorf("repair self related rows: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE relations SET src_id = dst_id, dst_id = src_id WHERE type='related-to' AND src_id > dst_id`); err != nil {
			return fmt.Errorf("repair related ordering: %w", err)
		}
		return nil
	}); err != nil {
		return storage.HealthReport{}, err
	}
	// The post-repair examination IS Doctor, not a copy of it, so what a repair
	// reports and what an examination reports cannot drift.
	// [LAW:one-source-of-truth]
	return s.Doctor(ctx)
}

// containmentFindings names every ticket the containment rule does not hold
// for (storage.RejectLeafWithoutParent, storage.RejectParentThatIsNotAContainer):
// a task, bug, feature or chore with no parent, and a ticket under a parent
// that is not an epic. Each finding names the one repair that fixes both, the
// `lit parent set` that replaces every parent of its child.
func containmentFindings(ctx context.Context, q rowQueryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
SELECT i.id, i.issue_type, '' FROM issues i
WHERE i.issue_type <> 'epic'
  AND NOT EXISTS (SELECT 1 FROM relations r WHERE r.src_id = i.id AND r.type = 'parent-child')
UNION ALL
SELECT r.src_id, p.issue_type, r.dst_id FROM relations r JOIN issues p ON p.id = r.dst_id
WHERE r.type = 'parent-child' AND p.issue_type <> 'epic'
ORDER BY 1, 3`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var loose, underLeaf []string
	for rows.Next() {
		var id, issueType, parentID string
		if err := rows.Scan(&id, &issueType, &parentID); err != nil {
			return nil, err
		}
		if parentID == "" {
			loose = append(loose, fmt.Sprintf("%s (%s)", id, issueType))
			continue
		}
		underLeaf = append(underLeaf, fmt.Sprintf("%s under %s (%s)", id, parentID, issueType))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	const repair = "move each into the epic it belongs to with 'lit parent set --child <id> --parent <epic-id>'"
	var findings []string
	if len(loose) > 0 {
		findings = append(findings, fmt.Sprintf("tickets outside an epic: %s (every ticket except an epic lives inside one; %s)", strings.Join(loose, ", "), repair))
	}
	if len(underLeaf) > 0 {
		findings = append(findings, fmt.Sprintf("tickets under a parent that is not an epic: %s (only an epic contains tickets; %s)", strings.Join(underLeaf, ", "), repair))
	}
	return findings, nil
}

// ReplaceFromExport is the restore: it clears the live tables and rewrites them
// from export as the store's rules stand now, under the ordinary self-retrying
// mutation pipeline, and says what landing it changed. Three rules apply that
// the backup itself cannot know: a restore never un-purges, every ticket lives
// inside an epic — a backup taken before v7 holds top-level leaves, and one
// taken before a purge can hold a child of the purged epic, which the cut
// leaves with no parent — and every rank key orders one issue.
// [LAW:single-enforcer] the cut is ApplyPurges and the rest is landExport, the
// same a sync landing applies.
func (s *Store) ReplaceFromExport(ctx context.Context, export model.Export) (storage.Landing, error) {
	var landing storage.Landing
	err := s.withStampedMutation(ctx, commitStamp{Message: "replace from export"}, func(ctx context.Context, tx *sql.Tx) error {
		cut, err := cutByHeldPurges(ctx, tx, export)
		if err != nil {
			return err
		}
		// Assigned on every attempt, so a retried mutation reports the changes
		// of the attempt that committed.
		var landed model.Export
		landed, landing, err = landExport(cut)
		if err != nil {
			return err
		}
		return writeWholeExportTx(ctx, tx, landed)
	})
	if err != nil {
		return storage.Landing{}, err
	}
	return landing, nil
}

// rebuildFromExport writes export as it stands, cut only by purges, for the
// recovery candidate. A rebuild is trusted only when it conserves its source —
// the verify gate counts its rows and compares its ids against the dump's — so
// it must not add the edges and the epic a restore's homing would; a ticket the
// source held outside an epic is left there for `lit doctor` to name.
func (s *Store) rebuildFromExport(ctx context.Context, export model.Export, stamp commitStamp) error {
	return s.withStampedMutation(ctx, stamp, func(ctx context.Context, tx *sql.Tx) error {
		cut, err := cutByHeldPurges(ctx, tx, export)
		if err != nil {
			return err
		}
		return writeWholeExportTx(ctx, tx, cut)
	})
}

// cutByHeldPurges returns export with the store's own purge records folded
// into its own and every row they name removed, so a backup taken before a
// purge lands without the purged ticket and the record survives the rewrite. A
// write never un-purges. [LAW:single-enforcer] the cut is ApplyPurges, the same
// one the merge makes.
func cutByHeldPurges(ctx context.Context, tx *sql.Tx, export model.Export) (model.Export, error) {
	held, err := listPurges(ctx, tx)
	if err != nil {
		return model.Export{}, err
	}
	return export.ApplyPurges(model.UnionPurges(held, export.Purges)), nil
}

// writeWholeExportTx clears the live tables and rewrites them from an export
// inside one transaction — the path for a caller who does NOT know what the
// tables currently hold (a restore, a rebuild) and so must establish the state
// wholesale.
//
// It is the degenerate case of applyExportDelta, not a second import body: the
// clear makes the live state provably empty, and the transition from empty to
// export is a delta with nothing to remove and everything to add. The whole-row
// INSERTs therefore still live in exactly one place each, shared with the
// reconcile's incremental replay — sole ownership of the restore-and-replay
// path, not of the tables: CreateIssue, AddComment and AddLabel keep their own
// creation-time statements with their own narrower column lists.
// [LAW:single-enforcer] [LAW:one-source-of-truth]
//
// The clear must not name issue_events or issue_event_changes: both cascade
// from issues (ON DELETE CASCADE), so clearing issues already removes them, and
// re-listing them here would be a second, drift-prone statement of a fact the
// schema owns.
func writeWholeExportTx(ctx context.Context, tx *sql.Tx, export model.Export) error {
	for _, table := range []string{"labels", "comments", "relations", "issues", "purges"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}
	return applyExportDelta(ctx, tx, diffExports(model.Export{}, export))
}

// insertIssueStmt is the restore/replay INSERT, kept beside issueRowValues so
// the column list and the value tuple that fills it are read and edited
// together — they must agree positionally, and separating them is how they
// stop agreeing.
//
// This is the whole-row writer, used by a restore and by the reconcile replay.
// It is NOT the only INSERT into issues: CreateIssue (store.go) writes a
// creation-time row with a narrower column list of its own. A schema change
// that adds a column has to be carried to both.
const insertIssueStmt = `INSERT INTO issues(id, title, description, agent_prompt, status, priority, issue_type, topic, assignee, item_rank, lane, follows_up, created_at, updated_at, closed_at, resolution, redirect_target, archived_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'misc'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// issueRowValues is the issues row an export issue becomes: every persisted
// column, in insertIssueStmt's order, with the three normalizations the storage
// layer owns rather than the caller — the container-vs-leaf status, the
// priority domain, and the topic slug.
//
// It exists as its own function because it has two consumers, and the second
// one is why the first is written this way. insertIssueTx binds it. The
// reconcile's delta COMPARES it, to decide whether an issue's row actually
// changed — and comparing this tuple is not the same as comparing the
// model.Issue it came from. A hydrated Issue carries `Labels`, denormalized
// from the labels table, and for a container a lifecycle composed from every
// child; neither is an issues column. Diffing whole Issue values therefore
// would call a row "changed" whenever a label moved or any child of an epic
// did, and rewrite that issue and everything ON DELETE CASCADE takes with it.
//
// Deriving the comparison from the writer keeps the property that comparing
// whole values has — nothing is compared by a hand-maintained field list that
// a new column could fall out of — while making the subject the row rather
// than the view of it. Add a column and it enters the write and the diff in the
// same edit, because they are the same tuple. [LAW:one-source-of-truth]
//
// Each normalization is a PURE function of the issue, which is what lets a
// comparison over export values stay faithful to the normalized rows on disk:
// equal inputs normalize equally, so an unchanged row is never missed.
func issueRowValues(issue model.Issue) []any {
	var closedAt any
	if value := issue.ClosedAtValue(); value != nil {
		closedAt = value.Format(time.RFC3339Nano)
	}
	// [LAW:single-enforcer] statusForStorage owns the container-vs-leaf
	// decision; the import path inherits it instead of inventing its own
	// default for containers.
	status := statusForStorage(issue)
	// Legacy exports may carry priorities outside the canonical
	// {normal, urgent} range. model.CanonicalPriority — the same
	// authority the live parse gate rejects against — coerces any such
	// value so the CHECK constraint can never reject a restore, without
	// the import path inventing its own notion of the domain.
	// [LAW:single-enforcer]
	priority := model.CanonicalPriority(int(issue.Priority))
	archivedCol, deletedCol := retentionColumns(issue)
	return []any{
		issue.ID, issue.Title, issue.Description, nullableString(issue.Prompt), status, priority,
		issue.IssueType, issueid.NormalizeSlug(issue.Topic), issue.AssigneeValue(), issue.Rank, issue.Lane, nullableString(issue.FollowsUp),
		issue.CreatedAt.Format(time.RFC3339Nano), issue.UpdatedAt.Format(time.RFC3339Nano), closedAt,
		nullableResolution(issue.ResolutionValue()), nullableStringPtr(issue.RedirectTargetValue()),
		archivedCol, deletedCol,
	}
}

func insertIssueTx(ctx context.Context, tx *sql.Tx, issue model.Issue) error {
	if _, err := tx.ExecContext(ctx, insertIssueStmt, issueRowValues(issue)...); err != nil {
		return fmt.Errorf("restore issue %s: %w", issue.ID, err)
	}
	return nil
}

func insertCommentTx(ctx context.Context, tx *sql.Tx, comment model.Comment) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO comments(id, issue_id, body, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
		comment.ID, comment.IssueID, comment.Body, comment.CreatedAt.Format(time.RFC3339Nano), comment.CreatedBy); err != nil {
		return fmt.Errorf("restore comment %s: %w", comment.ID, err)
	}
	return nil
}

func insertLabelTx(ctx context.Context, tx *sql.Tx, label model.Label) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO labels(issue_id, label, created_at, created_by) VALUES (?, ?, ?, ?)`,
		label.IssueID, label.Name, label.CreatedAt.Format(time.RFC3339Nano), label.CreatedBy); err != nil {
		return fmt.Errorf("restore label %s:%s: %w", label.IssueID, label.Name, err)
	}
	return nil
}

// insertEventTx writes an event together with its field changes: the nested
// rows are part of the event's value, so they land and leave with it (their
// table cascades from issue_events). It is the one writer of those tables —
// a live mutation's event, a reconcile's settled export and a restored dump
// all land here — so a change row naming no field is refused for each of them
// alike. [LAW:single-enforcer]
//
// The event's attribution is written exactly as the event carries it: newEvent
// stamped a live event from the store, and a restored one keeps what the dump
// carried — the
// checkout performing the restore never substitutes its own. Attribution is
// historical fact about who produced the work, so re-stamping here would
// rewrite history into a claim for whoever happened to run the restore, and
// preserving it is also what makes an export/import round trip lossless.
//
// Writing it verbatim is safe against a corrupted or hand-edited dump because
// model.Attribution.UnmarshalJSON already collapsed any half pair to the absent
// one on the way in. Re-checking here would be a second enforcer of that
// invariant, free to drift from the first and covering only this one consumer.
// [LAW:single-enforcer]
func insertEventTx(ctx context.Context, tx *sql.Tx, event model.IssueEvent) error {
	var actionArg any
	if event.Action != "" {
		actionArg = event.Action
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO issue_events(id, issue_id, action, reason, actor, created_at, stream_id, workspace_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.IssueID, actionArg, event.Reason, event.Actor, event.CreatedAt.Format(time.RFC3339Nano),
		nullableString(event.Attribution.Stream()), nullableString(event.Attribution.Workspace())); err != nil {
		return fmt.Errorf("insert issue event %s: %w", event.ID, err)
	}
	for _, change := range event.Changes {
		if strings.TrimSpace(change.Field) == "" {
			return fmt.Errorf("issue event %s: field name cannot be empty", event.ID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO issue_event_changes(event_id, field, from_value, to_value) VALUES (?, ?, ?, ?)`,
			event.ID, change.Field, nullableString(change.From), nullableString(change.To)); err != nil {
			return fmt.Errorf("insert issue event change %s.%s: %w", event.ID, change.Field, err)
		}
	}
	return nil
}
