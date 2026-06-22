package store

import (
	"io/fs"
	"regexp"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// This file holds the knowledge that changes as the schema's history grows: the
// correspondence between historical source-column names and domain fields, the
// fan-out shape of the legacy issue_history table, and the record of which
// columns past migrations intentionally removed. shapemap.go (the types and the
// applier) does not change when a new shape is learned; this file does. They are
// split by change-reason.

// DeterministicMap proposes a total ShapeMapping for a dump whose every table and
// column it recognizes, or returns ok=false when the dump carries a table or
// column outside its vocabulary — in which case the LLM/operator mapper takes
// over.
//
// [LAW:one-type-per-behavior] There are not separate "clean-ahead" and
// "pre-goose" mappers. Every simple domain table folds through one
// correspondence table into one Always emitter; the shapes differ only in which
// entries fire — clean-ahead needs no aliases, pre-goose exercises
// prompt/assignee aliases and the legacy-status transform.
// [LAW:single-enforcer] ok=true must mean "a valid mapping": the proposal routes
// through Validate rather than re-deriving its rules here. A dump mid-rename that
// carries both a v1 name and its pre-goose alias (e.g. prompt and agent_prompt)
// would map both onto one field — the field map keeps one, leaving the other
// column unaccounted, which Validate rejects — so the mapper declines it to the
// operator path instead of emitting a lossy mapping.
func DeterministicMap(dump RawDump) (ShapeMapping, bool) {
	out := ShapeMapping{}
	for _, table := range dump.Tables {
		tm, ok := mapTable(table)
		if !ok {
			return ShapeMapping{}, false
		}
		out.Tables = append(out.Tables, tm)
	}
	if Validate(dump, out) != nil {
		return ShapeMapping{}, false
	}
	return out, true
}

// mapTable dispositions one source table: bookkeeping tables drop wholesale, the
// legacy issue_history table fans out, a recognized domain table emits one
// record per row, and anything else declines to the operator path.
func mapTable(table RawTable) (TableMapping, bool) {
	if _, ok := bookkeepingTables[table.Name]; ok {
		return dropAllColumns(table), true
	}
	if table.Name == "issue_history" {
		return issueHistoryFanOut(table)
	}
	rules, ok := knownSourceColumns[table.Name]
	if !ok {
		return TableMapping{}, false
	}
	return simpleEmitter(table, rules)
}

// simpleEmitter builds the one Always emitter a recognized domain table needs:
// each column maps onto its domain field with that field's canonical transform.
// An unrecognized column declines the whole dump — a domain table carrying a
// column outside the vocabulary is the novel-shape case the operator path owns.
func simpleEmitter(table RawTable, rules map[string]TargetKey) (TableMapping, bool) {
	fields := map[string]FieldSource{}
	var coll collection
	for _, col := range table.Columns {
		target, known := rules[col]
		if !known {
			return TableMapping{}, false
		}
		tf := targetRegistry[target]
		coll = tf.coll
		fields[tf.field] = FromColumn{Column: col, Transform: tf.canonical}
	}
	return TableMapping{
		Table:    table.Name,
		Emitters: []Emitter{{Collection: coll, When: Always{}, Fields: fields}},
	}, true
}

// dropAllColumns dispositions every column of a bookkeeping table as an intended
// drop — infrastructure with no domain representation by design.
func dropAllColumns(table RawTable) TableMapping {
	drops := map[string]Dropped{}
	for _, col := range table.Columns {
		provenance, reason := classifyDrop(table.Name, col)
		drops[col] = Dropped{Provenance: provenance, Reason: reason}
	}
	return TableMapping{Table: table.Name, Drops: drops}
}

// issueHistoryFanOut expresses the legacy issue_history shape: each row becomes
// one issue_events row (always) plus — only for a real status transition — one
// issue_event_changes row carrying field="status". This mirrors the reconcile's
// translateIssueHistoryToEvents exactly: the event canonicalizers normalize
// action/reason/actor as recordEvent does, the legacy-status transform
// canonicalizes from/to, and WhenChanged on the (canonicalized) from/to is the
// isLegacyStatusTransition predicate.
//
// [LAW:one-source-of-truth] The presence gate is legacyIssueHistoryColumns — the
// same minimum column set the reconcile requires. A partial/synthetic
// issue_history (missing a canonical column) declines to the operator path rather
// than being guessed at; a strict superset declines too, because its extra
// columns would be unaccounted-for under this fan-out and an operator must decide
// their fate rather than have them silently dropped.
func issueHistoryFanOut(table RawTable) (TableMapping, bool) {
	cols := map[string]bool{}
	for _, c := range table.Columns {
		cols[c] = true
	}
	for _, c := range legacyIssueHistoryColumns {
		if !cols[c] {
			return TableMapping{}, false
		}
	}
	return TableMapping{
		Table: "issue_history",
		Emitters: []Emitter{
			{
				Collection: collEvents,
				When:       Always{},
				Fields: map[string]FieldSource{
					"id":         FromColumn{Column: "id", Transform: TransformIdentity},
					"issue_id":   FromColumn{Column: "issue_id", Transform: TransformIdentity},
					"action":     FromColumn{Column: "action", Transform: TransformEventAction},
					"reason":     FromColumn{Column: "reason", Transform: TransformEventReason},
					"actor":      FromColumn{Column: "created_by", Transform: TransformEventActor},
					"created_at": FromColumn{Column: "created_at", Transform: TransformTimestamp},
				},
			},
			{
				Collection: collEventChanges,
				When:       WhenChanged{FieldA: "from", FieldB: "to"},
				Fields: map[string]FieldSource{
					"event_id": FromColumn{Column: "id", Transform: TransformIdentity},
					"field":    Constant{Value: "status"},
					"from":     FromColumn{Column: "from_status", Transform: TransformLegacyStatus},
					"to":       FromColumn{Column: "to_status", Transform: TransformLegacyStatus},
				},
			},
		},
	}, true
}

// knownSourceColumns is the correspondence table: per domain source table, the
// domain target each recognized source column name maps onto. Aliases (a v1 name
// and its pre-goose predecessor) point at the same domain field; only one is
// present in any given dump, so they never collide. The transform is not named
// here — it is the target field's canonical transform, read from the registry —
// so this table is purely the source-name → domain-field correspondence.
//
// issue_history is NOT here: it is not a simple one-row-one-record table, so its
// shape lives in issueHistoryFanOut.
var knownSourceColumns = map[string]map[string]TargetKey{
	"issues": {
		"id":           "issues.id",
		"title":        "issues.title",
		"description":  "issues.description",
		"agent_prompt": "issues.prompt", // v1 name
		"prompt":       "issues.prompt", // pre-goose, pre-rename
		"status":       "issues.status",
		"priority":     "issues.priority",
		"issue_type":   "issues.issue_type",
		"topic":        "issues.topic",
		"assignee":     "issues.assignee",
		"created_at":   "issues.created_at",
		"updated_at":   "issues.updated_at",
		"closed_at":    "issues.closed_at",
		"resolution":   "issues.resolution",
		"archived_at":  "issues.archived_at",
		"deleted_at":   "issues.deleted_at",
		"item_rank":    "issues.rank", // v1 name
		"lane":         "issues.lane",
	},
	"relations": {
		"src_id":     "relations.src_id",
		"dst_id":     "relations.dst_id",
		"type":       "relations.type",
		"created_at": "relations.created_at",
		"created_by": "relations.created_by",
	},
	"comments": {
		"id":         "comments.id",
		"issue_id":   "comments.issue_id",
		"body":       "comments.body",
		"created_at": "comments.created_at",
		"created_by": "comments.created_by",
	},
	"labels": {
		"issue_id":   "labels.issue_id",
		"label":      "labels.name",
		"created_at": "labels.created_at",
		"created_by": "labels.created_by",
	},
	"issue_events": {
		"id":         "events.id",
		"issue_id":   "events.issue_id",
		"action":     "events.action",
		"reason":     "events.reason",
		"actor":      "events.actor", // v1 name
		"assignee":   "events.actor", // pre-goose, pre-rename
		"created_at": "events.created_at",
	},
	"issue_event_changes": {
		"event_id":   "event_changes.event_id",
		"field":      "event_changes.field",
		"from_value": "event_changes.from",
		"to_value":   "event_changes.to",
	},
}

// bookkeepingTables are the tables a dump carries that have no domain
// representation by design: their columns drop, intended, with the reason stated.
// They are infrastructure, not application data.
var bookkeepingTables = map[string]string{
	"goose_db_version":     "goose migration registry — schema bookkeeping, no domain field",
	"migration_quarantine": "migration quarantine ledger — schema bookkeeping, no domain field",
	"meta":                 "schema metadata table — no domain field",
}

// classifyDrop decides why a source column has no domain target, distinguishing
// the silent-pass case (intended) from the surface-to-human case (unexplained)
// from migration history. [LAW:single-enforcer] One definition of "why was this
// dropped" serves every mapper and the operator path alike.
//
// A column is an intended drop when its table is bookkeeping (no domain field by
// design) or when a numbered migration explicitly removed it; otherwise the drop
// is unexplained.
func classifyDrop(table, column string) (DropProvenance, string) {
	if reason, ok := bookkeepingTables[table]; ok {
		return DropIntended, reason
	}
	if file, ok := migrationDroppedCols[ColumnRef{Table: table, Column: column}]; ok {
		return DropIntended, "removed by migration " + file
	}
	return DropUnexplained, ""
}

// ColumnRef names one source column positionally within a dump: a table and a
// column within it. It keys the migration-drop index and the operator-facing
// list of unexplained drops.
type ColumnRef struct {
	Table  string
	Column string
}

func (c ColumnRef) String() string { return c.Table + "." + c.Column }

// alterTableRE captures one ALTER TABLE statement: its target table and the
// statement body up to the terminating semicolon (or end of input). The body is
// then scanned for every DROP COLUMN clause, so a multi-drop statement is fully
// captured and a later statement's drops are never attributed to this table.
var alterTableRE = regexp.MustCompile("(?is)ALTER\\s+TABLE\\s+`?(\\w+)`?([^;]*)")
var dropColumnRE = regexp.MustCompile("(?is)DROP\\s+COLUMN\\s+(?:IF\\s+EXISTS\\s+)?`?(\\w+)`?")

// parseDroppedColumns extracts every (table, column) a chunk of migration SQL
// explicitly drops — including multiple DROP COLUMN clauses in one ALTER TABLE
// statement. It is a pure function so the "migration history" arm of classifyDrop
// is testable without depending on the current corpus happening to contain a
// drop.
func parseDroppedColumns(sqlText string) []ColumnRef {
	var refs []ColumnRef
	for _, stmt := range alterTableRE.FindAllStringSubmatch(sqlText, -1) {
		table, body := stmt[1], stmt[2]
		for _, drop := range dropColumnRE.FindAllStringSubmatch(body, -1) {
			refs = append(refs, ColumnRef{Table: table, Column: drop[1]})
		}
	}
	return refs
}

// migrationDroppedCols maps every column a numbered migration removed to the file
// that removed it. It is scanned once from the embedded goose corpus. The
// pre-goose renames (prompt→agent_prompt, assignee→actor) are not drops — the
// values survive under a new name, so the correspondence table maps them. A
// genuine future DROP COLUMN migration is the only way a domain column becomes an
// intended drop.
var migrationDroppedCols = scanMigrationDrops()

func scanMigrationDrops() map[ColumnRef]string {
	out := map[ColumnRef]string{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		// The corpus is embedded at build time; a read failure here is an
		// impossible state, not a runtime condition to recover from.
		panic("scan migration drops: read embedded registry: " + err.Error())
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := migrations.FS.ReadFile(entry.Name())
		if err != nil {
			panic("scan migration drops: read " + entry.Name() + ": " + err.Error())
		}
		// [LAW:one-source-of-truth] Only the forward (Up) section records intended
		// drops. A goose Down section is the inverse of Up — it DROPs the columns Up
		// ADDs — so scanning it would misclassify a kept column as an intended drop
		// and could justify discarding live data.
		for _, ref := range parseDroppedColumns(gooseUpSection(string(data))) {
			out[ref] = entry.Name()
		}
	}
	return out
}
