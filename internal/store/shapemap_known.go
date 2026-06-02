package store

import (
	"io/fs"
	"regexp"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

// This file holds the knowledge that changes as the schema's history grows:
// the correspondence between historical source-column names and domain fields,
// and the record of which columns past migrations intentionally removed.
// shapemap.go (the type and the applier) does not change when a new shape is
// learned; this file does. They are split by change-reason.

// DeterministicMap proposes a total ShapeMapping for a dump whose every column
// it recognizes, or returns ok=false when the dump carries a column or table
// outside its vocabulary — in which case the LLM mapper (wired in the loop
// ticket) takes over.
//
// [LAW:one-type-per-behavior] There are not separate "clean-ahead" and
// "pre-goose" mappers. They are the same fold over one correspondence table;
// the shapes differ only in which entries fire — clean-ahead needs no aliases,
// pre-goose exercises prompt/assignee aliases and the legacy-status transform.
// [LAW:dataflow-not-control-flow] The status column carries the legacy
// transform unconditionally because that transform is idempotent on canonical
// values, so no "is this pre-goose?" branch is needed.
func DeterministicMap(dump RawDump) (ShapeMapping, bool) {
	out := ShapeMapping{Columns: map[ColumnRef]Disposition{}}
	for _, table := range dump.Tables {
		rules, isDomain := knownSourceColumns[table.Name]
		_, isBookkeeping := bookkeepingTables[table.Name]
		for _, col := range table.Columns {
			ref := ColumnRef{Table: table.Name, Column: col}
			switch {
			case isBookkeeping:
				provenance, reason := classifyDrop(table.Name, col)
				out.Columns[ref] = Dropped{Provenance: provenance, Reason: reason}
			case isDomain:
				disp, known := rules[col]
				if !known {
					// A domain table carrying a column we don't recognize is
					// exactly the novel-shape case: decline so the loop's LLM
					// mapper can reason about it rather than guess here.
					return ShapeMapping{}, false
				}
				out.Columns[ref] = disp
			default:
				return ShapeMapping{}, false
			}
		}
	}
	// [LAW:single-enforcer] ok=true must mean "a valid mapping": route the
	// proposal through Validate rather than re-deriving its rules here. A dump
	// mid-rename that carries both a v1 name and its pre-goose alias (e.g.
	// prompt and agent_prompt) maps both onto one field — an ambiguity Validate
	// rejects — so the mapper declines it to the loop's LLM/human path instead
	// of emitting a lossy mapping.
	if Validate(dump, out) != nil {
		return ShapeMapping{}, false
	}
	return out, true
}

// to maps a source column onto a domain target field. The value conversion is
// not named here — it is a property of the target, read from the target
// registry — so this table is purely the source-name → domain-field
// correspondence.
func to(target TargetKey) Disposition { return MappedTo{Target: target} }

// knownSourceColumns is the correspondence table: per domain source table, the
// domain field each recognized source column name maps onto. Aliases (a v1 name
// and its pre-goose predecessor) point at the same domain field; only one is
// present in any given dump, so they never collide.
var knownSourceColumns = map[string]map[string]Disposition{
	"issues": {
		"id":           to("issues.id"),
		"title":        to("issues.title"),
		"description":  to("issues.description"),
		"agent_prompt": to("issues.prompt"), // v1 name
		"prompt":       to("issues.prompt"), // pre-goose, pre-rename
		"status":       to("issues.status"),
		"priority":     to("issues.priority"),
		"issue_type":   to("issues.issue_type"),
		"topic":        to("issues.topic"),
		"assignee":     to("issues.assignee"),
		"created_at":   to("issues.created_at"),
		"updated_at":   to("issues.updated_at"),
		"closed_at":    to("issues.closed_at"),
		"archived_at":  to("issues.archived_at"),
		"deleted_at":   to("issues.deleted_at"),
		"item_rank":    to("issues.rank"), // v1 name
	},
	"relations": {
		"src_id":     to("relations.src_id"),
		"dst_id":     to("relations.dst_id"),
		"type":       to("relations.type"),
		"created_at": to("relations.created_at"),
		"created_by": to("relations.created_by"),
	},
	"comments": {
		"id":         to("comments.id"),
		"issue_id":   to("comments.issue_id"),
		"body":       to("comments.body"),
		"created_at": to("comments.created_at"),
		"created_by": to("comments.created_by"),
	},
	"labels": {
		"issue_id":   to("labels.issue_id"),
		"label":      to("labels.name"),
		"created_at": to("labels.created_at"),
		"created_by": to("labels.created_by"),
	},
	"issue_events": {
		"id":         to("events.id"),
		"issue_id":   to("events.issue_id"),
		"action":     to("events.action"),
		"reason":     to("events.reason"),
		"actor":      to("events.actor"), // v1 name
		"assignee":   to("events.actor"), // pre-goose, pre-rename
		"created_at": to("events.created_at"),
	},
	"issue_event_changes": {
		"event_id":   to("event_changes.event_id"),
		"field":      to("event_changes.field"),
		"from_value": to("event_changes.from"),
		"to_value":   to("event_changes.to"),
	},
}

// bookkeepingTables are the tables a dump carries that have no domain
// representation by design: their columns drop, intended, with the reason
// stated. They are infrastructure, not application data.
var bookkeepingTables = map[string]string{
	"goose_db_version":     "goose migration registry — schema bookkeeping, no domain field",
	"migration_quarantine": "migration quarantine ledger — schema bookkeeping, no domain field",
	"meta":                 "schema metadata table — no domain field",
}

// classifyDrop decides why a source column has no domain target, distinguishing
// the silent-pass case (intended) from the surface-to-human case (unexplained)
// from migration history. [LAW:single-enforcer] One definition of "why was this
// dropped" serves every mapper and the LLM path alike.
//
// A column is an intended drop when its table is bookkeeping (no domain field
// by design) or when a numbered migration explicitly removed it; otherwise the
// drop is unexplained.
func classifyDrop(table, column string) (DropProvenance, string) {
	if reason, ok := bookkeepingTables[table]; ok {
		return DropIntended, reason
	}
	if file, ok := migrationDroppedCols[ColumnRef{Table: table, Column: column}]; ok {
		return DropIntended, "removed by migration " + file
	}
	return DropUnexplained, ""
}

// alterTableRE captures one ALTER TABLE statement: its target table and the
// statement body up to the terminating semicolon (or end of input). The body
// is then scanned for every DROP COLUMN clause, so a multi-drop statement is
// fully captured and a later statement's drops are never attributed to this
// table.
var alterTableRE = regexp.MustCompile("(?is)ALTER\\s+TABLE\\s+`?(\\w+)`?([^;]*)")
var dropColumnRE = regexp.MustCompile("(?is)DROP\\s+COLUMN\\s+(?:IF\\s+EXISTS\\s+)?`?(\\w+)`?")

// parseDroppedColumns extracts every (table, column) a chunk of migration SQL
// explicitly drops — including multiple DROP COLUMN clauses in one ALTER TABLE
// statement. It is a pure function so the "migration history" arm of
// classifyDrop is testable without depending on the current corpus happening to
// contain a drop.
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

// migrationDroppedCols maps every column a numbered migration removed to the
// file that removed it. It is scanned once from the embedded goose corpus. The
// pre-goose renames (prompt→agent_prompt, assignee→actor) are not drops — the
// values survive under a new name, so the correspondence table maps them. A
// genuine future DROP COLUMN migration is the only way a domain column becomes
// an intended drop.
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
		// [LAW:one-source-of-truth] Only the forward (Up) section records
		// intended drops. A goose Down section is the inverse of Up — it DROPs
		// the columns Up ADDs — so scanning it would misclassify a kept column
		// as an intended drop and could justify discarding live data.
		for _, ref := range parseDroppedColumns(gooseUpSection(string(data))) {
			out[ref] = entry.Name()
		}
	}
	return out
}
