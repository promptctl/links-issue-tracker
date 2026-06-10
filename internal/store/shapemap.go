package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// ShapeMapping is the seam between reasoning (an LLM, or a deterministic mapper)
// and mechanism (the applier). It is the declarative artifact that says, for one
// RawDump, how every source row becomes domain data.
//
// [LAW:types-are-the-program] The unit of a mapping is the RECORD-EMITTER, not
// the column. A source table declares the records it emits (one per emitter per
// row that the emitter's condition admits) and, separately, the columns it
// discards. A simple one-table-one-collection shape is the degenerate case: one
// Always emitter whose fields are all FromColumn. Fan-out — one table feeding
// several collections, one column feeding several fields, a synthesized
// conditional child record — is the same shape with more emitters. The applier
// does not branch on which case it is.
//
// [LAW:types-are-the-program] A mapping is a TOTAL disposition over the source
// columns: every column of every dumped table is either referenced by some
// emitter's FromColumn source or recorded as a Dropped column with a provenance.
// Totality is free data a producer can get wrong (a column simply unmentioned),
// so it is enforced once at the Validate trust boundary; past that boundary the
// applier assumes it. "Silently lost a column" is therefore not a state any
// applied mapping can be in — it is rejected before apply.
type ShapeMapping struct {
	Tables []TableMapping
}

// TableMapping is one source table's complete disposition: the records its rows
// emit, and the columns it discards. [LAW:types-are-the-program] The two fates a
// column can have — feed a record, or be dropped — are the only two places a
// column name appears, so a column with neither fate (silent loss) or both
// (contradiction) is caught at Validate, not defended against in Apply.
type TableMapping struct {
	Table    string
	Emitters []Emitter
	// Drops records columns with no domain target, keyed by column name within
	// this table. A drop carries its provenance so "notify only on drops" can
	// distinguish an intended drop (silent) from an unexplained one (the
	// surface-to-human case).
	Drops map[string]Dropped
}

// Emitter produces records of one collection: one record for each source row
// whose computed fields satisfy When. [LAW:types-are-the-program] The condition
// is DATA (a value in the sealed EmitCondition set), evaluated uniformly by the
// applier every row — so "this child record exists only for a real transition"
// is expressed as a value, not as a branch in the applier body.
type Emitter struct {
	Collection collection
	// Fields maps each domain field of the collection to where its value comes
	// from. A map keyed by field name makes "two sources fill the same field"
	// structurally unrepresentable — the duplicate-target fault the column-keyed
	// model had to reject is gone by construction.
	Fields map[string]FieldSource
	When   EmitCondition
}

// FieldSource is the closed set of ways one record field gets its value: from a
// source column (converted by a Transform) or a constant. [LAW:types-are-the-program]
// The sealed interface (only this package implements it) makes any third shape
// unrepresentable, so the applier's field fold is total over exactly two cases.
type FieldSource interface{ isFieldSource() }

// FromColumn fills a field from a source column, converting via Transform. The
// transform lives on the EDGE (source→target), not the target alone, because the
// same target field legitimately needs different transforms from different
// sources: event_changes.from is identity from a canonical issue_event_changes
// dump but legacy-status-canonicalized from a legacy issue_history dump. Validate
// constrains the choice to the target's admissible set, so a timestamp field can
// never be tagged identity.
type FromColumn struct {
	Column    string
	Transform Transform
}

// Constant fills a field with a fixed value independent of any source column —
// the synthesized literal a fan-out needs (issue_history's change rows all carry
// field="status"). Validate restricts constants to string-valued identity fields,
// so a constant cannot smuggle an unparsed value past a timestamp or status
// target.
type Constant struct {
	Value any
}

func (FromColumn) isFieldSource() {}
func (Constant) isFieldSource()   {}

// EmitCondition is the closed set of per-row emit decisions. [LAW:types-are-the-program]
// Always is the primary-record case; WhenChanged is the conditional-child case.
// A new condition is a new variant here (a documented, capped extension), never
// an ad-hoc predicate in the applier.
type EmitCondition interface{ isEmitCondition() }

// Always emits a record for every source row.
type Always struct{}

// WhenChanged emits only when two of the emitter's OWN computed fields differ,
// with SQL NULL counted as a distinct value — the "a real change occurred"
// shape. It references the computed fields (not raw columns) so the comparison
// runs on the post-transform values: with from/to carrying the legacy-status
// transform this is exactly isLegacyStatusTransition, without restating the
// transform (one source of truth for "what the canonical value is").
type WhenChanged struct {
	FieldA string
	FieldB string
}

func (Always) isEmitCondition()      {}
func (WhenChanged) isEmitCondition() {}

// Dropped records that a source column has no domain target, and why. The
// provenance is the discriminator that makes "notify only on drops" meaningful:
// an intended drop passes silently; an unexplained one is the surface-to-human
// case.
type Dropped struct {
	Provenance DropProvenance
	Reason     string
}

// DropProvenance discriminates a drop's justification.
type DropProvenance string

const (
	// DropIntended: the target baseline has no place for this column on purpose
	// — a migration removed it, or it is bookkeeping with no domain
	// representation by design. Mapping it to nothing is correct.
	DropIntended DropProvenance = "intended"
	// DropUnexplained: no target and no recorded reason. The genuine
	// surface-to-human case.
	DropUnexplained DropProvenance = "unexplained"
)

// Transform names a value conversion applied to a source cell as it lands on a
// domain field. The set is closed and small; a known shape adds a transform only
// when a real historical value vocabulary requires one. Each transform is total
// over its input including SQL NULL — there is no universal NULL short-circuit,
// because the canonicalizing transforms coerce NULL deliberately (reason→"",
// actor→"unknown") while the structural ones preserve it.
type Transform string

const (
	TransformIdentity     Transform = "identity"
	TransformLegacyStatus Transform = "legacy_status_value"
	TransformTimestamp    Transform = "timestamp"
	// The event canonicalizers mirror recordEvent's live-write normalization so a
	// translated legacy row is byte-equivalent to a live-written one.
	// [LAW:one-source-of-truth] They delegate to the reconcile's canonical*
	// functions rather than re-spelling the rules.
	TransformEventAction Transform = "event_action"
	TransformEventReason Transform = "event_reason"
	TransformEventActor  Transform = "event_actor"
)

// TargetKey is "<collection>.<field>" and resolves in targetRegistry.
type TargetKey string

type collection string

const (
	collIssues       collection = "issues"
	collRelations    collection = "relations"
	collComments     collection = "comments"
	collLabels       collection = "labels"
	collEvents       collection = "events"
	collEventChanges collection = "event_changes"
)

type targetField struct {
	coll  collection
	field string
	// canonical is the transform a simple 1:1 mapping uses for this field; admits
	// is the full set of transforms Validate accepts on an edge landing here
	// (always a superset containing canonical). Most fields admit only their
	// canonical transform; the few fed from heterogeneous sources (the event and
	// event_changes value fields) admit a small documented set.
	canonical Transform
	admits    map[Transform]bool
	// optional is true when an absent source for this target is legitimate (the
	// field is nullable or has a write-path default), so the recognizable-shape
	// gate does not require an emitter to cover it. A required target absent from
	// an emitter would force Apply to fabricate an empty value, so the gate
	// rejects such an emitter.
	optional bool
}

// targetRegistry is the closed set of legal mapping targets: the
// version-independent domain contract (model.Export's collections), enumerated
// once. [LAW:one-source-of-truth] Valid targets, their admissible transforms,
// their canonical transform, and their requiredness all live here; Validate
// consults this set and the applier dispatches on it, so neither invents its own
// idea of "what a field is".
//
// event_changes.event_id is the one join-only target: the applier consumes it to
// nest a change under its event; it is not a model.FieldChange field.
var targetRegistry = buildTargetRegistry()

// knownCollections is the closed set of collections any emitter may target,
// derived from the registry so it cannot drift from the targets that exist.
var knownCollections = buildKnownCollections()

const (
	required = false // !optional
	optional = true
)

func buildTargetRegistry() map[TargetKey]targetField {
	reg := map[TargetKey]targetField{}
	// add registers fields whose only admissible transform is the canonical one.
	add := func(c collection, canonical Transform, opt bool, fields ...string) {
		for _, f := range fields {
			reg[TargetKey(string(c)+"."+f)] = targetField{
				coll: c, field: f, canonical: canonical,
				admits: map[Transform]bool{canonical: true}, optional: opt,
			}
		}
	}
	// addMulti registers a field that legitimately accepts more than one transform
	// depending on its source. The canonical transform is what a simple mapping
	// uses; extra transforms are admissible when a richer source (legacy
	// issue_history fan-out) requires canonicalization the canonical sources
	// already had applied.
	addMulti := func(c collection, canonical Transform, extra []Transform, opt bool, field string) {
		admits := map[Transform]bool{canonical: true}
		for _, x := range extra {
			admits[x] = true
		}
		reg[TargetKey(string(c)+"."+field)] = targetField{
			coll: c, field: field, canonical: canonical, admits: admits, optional: opt,
		}
	}
	add(collIssues, TransformIdentity, required, "id", "title", "description", "priority", "issue_type")
	add(collIssues, TransformIdentity, optional, "prompt", "assignee", "topic", "rank", "lane")
	add(collIssues, TransformTimestamp, required, "created_at", "updated_at", "closed_at")
	add(collIssues, TransformTimestamp, optional, "archived_at", "deleted_at")
	add(collIssues, TransformLegacyStatus, required, "status")
	add(collRelations, TransformIdentity, required, "src_id", "dst_id", "type", "created_by")
	add(collRelations, TransformTimestamp, required, "created_at")
	add(collComments, TransformIdentity, required, "id", "issue_id", "body", "created_by")
	add(collComments, TransformTimestamp, required, "created_at")
	add(collLabels, TransformIdentity, required, "issue_id", "name", "created_by")
	add(collLabels, TransformTimestamp, required, "created_at")
	add(collEvents, TransformIdentity, required, "id", "issue_id")
	addMulti(collEvents, TransformIdentity, []Transform{TransformEventAction}, optional, "action")
	addMulti(collEvents, TransformIdentity, []Transform{TransformEventReason}, required, "reason")
	addMulti(collEvents, TransformIdentity, []Transform{TransformEventActor}, required, "actor")
	add(collEvents, TransformTimestamp, required, "created_at")
	add(collEventChanges, TransformIdentity, required, "event_id", "field")
	addMulti(collEventChanges, TransformIdentity, []Transform{TransformLegacyStatus}, optional, "from")
	addMulti(collEventChanges, TransformIdentity, []Transform{TransformLegacyStatus}, optional, "to")
	return reg
}

func buildKnownCollections() map[collection]bool {
	out := map[collection]bool{}
	for _, tf := range targetRegistry {
		out[tf.coll] = true
	}
	return out
}

// Validate is the single well-formedness enforcer. [LAW:single-enforcer] It is
// the one trust boundary that decides whether a mapping (from an LLM, an
// operator, or a deterministic mapper) may be applied. It rejects:
//   - a source column with neither an emitter reference nor a drop, or referenced
//     by an emitter AND dropped (totality, exactly-one-fate);
//   - a mapping naming a table or column the dump does not have;
//   - an emitter into an unknown collection, or onto an unknown target field;
//   - a FromColumn whose transform the target does not admit, or a Constant on a
//     non-string / non-identity field;
//   - an emitter that does not cover its collection's required fields;
//   - a WhenChanged naming fields the emitter does not produce;
//   - a row whose cell count does not match its column count.
//
// After Validate succeeds, Apply treats the mapping as total and well-formed.
func Validate(dump RawDump, m ShapeMapping) error {
	dumpTables, err := indexDumpTables(dump)
	if err != nil {
		return err
	}
	mapTables, err := indexMapping(m)
	if err != nil {
		return err
	}

	// Totality first: a column referenced by no emitter and recorded in no drop is
	// silent loss. The aggregated "not total" error names every unaccounted column
	// as "<table>.<column>" — this is the operator's worklist for the next
	// edit-and-rerun, so its wording is a contract, not a diagnostic detail. A
	// dump table with no mapping at all surfaces here as all its columns
	// unaccounted, not as a separate message.
	var unaccounted []string
	for name, table := range dumpTables {
		tm := mapTables[name]
		referenced := referencedColumns(tm)
		for _, col := range table.Columns {
			_, dropped := tm.Drops[col]
			if !referenced[col] && !dropped {
				unaccounted = append(unaccounted, name+"."+col)
			}
		}
	}
	if len(unaccounted) > 0 {
		sort.Strings(unaccounted)
		return fmt.Errorf("mapping is not total: %d source column(s) unaccounted for: %s",
			len(unaccounted), strings.Join(unaccounted, ", "))
	}

	var problems []string
	for name := range mapTables {
		if _, ok := dumpTables[name]; !ok {
			problems = append(problems, fmt.Sprintf("table %q: mapping references a table the dump does not have", name))
		}
	}
	for name, table := range dumpTables {
		tm, ok := mapTables[name]
		if !ok {
			continue
		}
		problems = append(problems, tableProblems(table, tm)...)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("mapping is malformed: %s", strings.Join(problems, "; "))
	}

	// [LAW:types-are-the-program] Apply indexes cells positionally, so "every row
	// has exactly one cell per column" is a precondition it relies on. Enforce it
	// here — the dump may be a serialized artifact from outside this process — so
	// a short or long row is a clear error at the boundary, not a panic deep in
	// Apply.
	for _, table := range dump.Tables {
		for i, row := range table.Rows {
			if len(row) != len(table.Columns) {
				return fmt.Errorf("table %q row %d has %d cells, want %d (one per column)",
					table.Name, i, len(row), len(table.Columns))
			}
		}
	}
	return nil
}

// referencedColumns is the set of columns some emitter draws a value from. A
// column in this set is "used" — the other legal fate is being dropped.
func referencedColumns(tm TableMapping) map[string]bool {
	referenced := map[string]bool{}
	for _, em := range tm.Emitters {
		for _, src := range em.Fields {
			if fc, ok := src.(FromColumn); ok {
				referenced[fc.Column] = true
			}
		}
	}
	return referenced
}

// tableProblems reports the well-formedness faults of one table's mapping that
// totality (checked in Validate) does not: the validity of each emitter, and each
// drop's exactly-one-fate, provenance, and existence. A column being both mapped
// and dropped is the contradiction caught here.
func tableProblems(table RawTable, tm TableMapping) []string {
	var problems []string
	cols := map[string]bool{}
	for _, c := range table.Columns {
		cols[c] = true
	}
	referenced := referencedColumns(tm)
	for _, em := range tm.Emitters {
		problems = append(problems, emitterProblems(table.Name, cols, em)...)
	}
	for col, d := range tm.Drops {
		if !cols[col] {
			problems = append(problems, fmt.Sprintf("table %q: drop names column %q the dump does not have", table.Name, col))
		}
		if referenced[col] {
			problems = append(problems, fmt.Sprintf("table %q column %q is both mapped and dropped", table.Name, col))
		}
		if d.Provenance != DropIntended && d.Provenance != DropUnexplained {
			problems = append(problems, fmt.Sprintf("table %q column %q: unknown drop provenance %q", table.Name, col, d.Provenance))
		}
	}
	return problems
}

// emitterProblems validates one emitter against the registry: its collection,
// each field's target and source, required-field coverage, and its condition.
func emitterProblems(tableName string, cols map[string]bool, em Emitter) []string {
	var problems []string
	if !knownCollections[em.Collection] {
		problems = append(problems, fmt.Sprintf("table %q: emitter into unknown collection %q", tableName, em.Collection))
		return problems
	}
	for field, src := range em.Fields {
		key := TargetKey(string(em.Collection) + "." + field)
		tf, ok := targetRegistry[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("table %q: emitter into %q targets unknown field %q", tableName, em.Collection, field))
			continue
		}
		switch s := src.(type) {
		case FromColumn:
			if !cols[s.Column] {
				problems = append(problems, fmt.Sprintf("table %q: %q.%q maps from column %q the dump does not have", tableName, em.Collection, field, s.Column))
			}
			if !tf.admits[s.Transform] {
				problems = append(problems, fmt.Sprintf("table %q: %q.%q does not admit transform %q", tableName, em.Collection, field, s.Transform))
			}
		case Constant:
			if tf.canonical != TransformIdentity {
				problems = append(problems, fmt.Sprintf("table %q: %q.%q is not a passthrough field; a constant cannot land here", tableName, em.Collection, field))
			}
			if _, ok := s.Value.(string); !ok {
				problems = append(problems, fmt.Sprintf("table %q: %q.%q constant must be a string, got %T", tableName, em.Collection, field, s.Value))
			}
		default:
			problems = append(problems, fmt.Sprintf("table %q: %q.%q has unknown field source %T", tableName, em.Collection, field, src))
		}
	}
	for key, tf := range targetRegistry {
		if tf.coll == em.Collection && !tf.optional {
			if _, ok := em.Fields[tf.field]; !ok {
				problems = append(problems, fmt.Sprintf("table %q: emitter into %q does not cover required field %q", tableName, em.Collection, key))
			}
		}
	}
	if w, ok := em.When.(WhenChanged); ok {
		for _, f := range []string{w.FieldA, w.FieldB} {
			if _, ok := em.Fields[f]; !ok {
				problems = append(problems, fmt.Sprintf("table %q: emitter condition references field %q the emitter does not produce", tableName, f))
			}
		}
	}
	return problems
}

// indexDumpTables maps table name to its RawTable, rejecting a dump that lists
// one table name twice (positional cells could not be attributed unambiguously).
func indexDumpTables(dump RawDump) (map[string]RawTable, error) {
	out := make(map[string]RawTable, len(dump.Tables))
	for _, t := range dump.Tables {
		if _, dup := out[t.Name]; dup {
			return nil, fmt.Errorf("dump lists table %q more than once", t.Name)
		}
		out[t.Name] = t
	}
	return out, nil
}

// indexMapping maps table name to its TableMapping, rejecting a mapping that
// dispositions one table twice (which fate wins would be ambiguous). It is the
// Validate-time index; post-Validate consumers use tablesByName.
func indexMapping(m ShapeMapping) (map[string]TableMapping, error) {
	out := make(map[string]TableMapping, len(m.Tables))
	for _, tm := range m.Tables {
		if _, dup := out[tm.Table]; dup {
			return nil, fmt.Errorf("mapping dispositions table %q more than once", tm.Table)
		}
		out[tm.Table] = tm
	}
	return out, nil
}

// tablesByName indexes a VALIDATED mapping by table name. Validate has already
// rejected duplicate table dispositions, so this pure builder is unambiguous; it
// exists so the applier and the conservation gate index without re-handling an
// error the boundary already excluded.
func tablesByName(m ShapeMapping) map[string]TableMapping {
	out := make(map[string]TableMapping, len(m.Tables))
	for _, tm := range m.Tables {
		out[tm.Table] = tm
	}
	return out
}

// Apply is the pure applier: it folds a RawDump through a validated mapping into
// a model.Export — the version-independent domain contract that ReplaceFromExport
// loads onto a fresh baseline. It assumes nothing about which producer wrote the
// mapping; deterministic, operator, and LLM mappers route through the identical
// fold.
//
// [LAW:dataflow-not-control-flow] The same fold runs for every emitter of every
// row of every table; the mapping's values — fields, transforms, and the When
// condition — are the only variability. Whether a row contributes a record to a
// collection is decided by the When VALUE evaluated against the computed record,
// never by a per-shape branch: the record is always built; only its membership
// in the collection varies, the way a WHERE clause filters rows.
func Apply(dump RawDump, m ShapeMapping) (model.Export, error) {
	if err := Validate(dump, m); err != nil {
		return model.Export{}, err
	}
	// Validate has already rejected a duplicate table disposition, so the
	// last-wins builder is unambiguous here.
	mapTables := tablesByName(m)
	records := map[collection][]map[string]any{}
	for _, table := range dump.Tables {
		tm := mapTables[table.Name]
		colIndex := rowColumnIndex(table)
		for _, row := range table.Rows {
			for _, em := range tm.Emitters {
				rec, err := buildRecord(em, table.Name, colIndex, row)
				if err != nil {
					return model.Export{}, fmt.Errorf("table %q: %w", table.Name, err)
				}
				if emits(em.When, rec) {
					records[em.Collection] = append(records[em.Collection], rec)
				}
			}
		}
	}
	return assembleExport(dump.WorkspaceID, records)
}

// columnIndex maps a table's column names to their positional index in a row.
func rowColumnIndex(table RawTable) map[string]int {
	idx := make(map[string]int, len(table.Columns))
	for i, name := range table.Columns {
		idx[name] = i
	}
	return idx
}

// buildRecord computes one emitter's record from one source row: each field's
// value is its FromColumn cell run through the transform, or its Constant value.
func buildRecord(em Emitter, tableName string, colIndex map[string]int, row []any) (map[string]any, error) {
	rec := make(map[string]any, len(em.Fields))
	for field, src := range em.Fields {
		switch s := src.(type) {
		case FromColumn:
			value, err := applyTransform(s.Transform, row[colIndex[s.Column]])
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", s.Column, err)
			}
			rec[field] = value
		case Constant:
			rec[field] = s.Value
		}
	}
	return rec, nil
}

// emits evaluates an emitter's condition against its computed record.
// [LAW:dataflow-not-control-flow] The condition is a value; this is the one place
// it is read, and it decides membership, not whether work runs.
func emits(when EmitCondition, rec map[string]any) bool {
	switch w := when.(type) {
	case WhenChanged:
		return cellsDiffer(rec[w.FieldA], rec[w.FieldB])
	default:
		// Always (and, by sealing, no other variant) emits unconditionally.
		return true
	}
}

// cellsDiffer reports whether two computed cells represent different values, with
// SQL NULL (nil) counted as a value distinct from any string. This is exactly the
// status-transition predicate (isLegacyStatusTransition) when the cells are the
// legacy-status-canonicalized from/to values.
func cellsDiffer(a, b any) bool {
	an, bn := a == nil, b == nil
	if an != bn {
		return true
	}
	if an {
		return false
	}
	return cellString(a) != cellString(b)
}

// assembleExport turns the collected per-collection records into the typed domain
// structs of a model.Export. event_changes records are nested under their events
// by event_id.
func assembleExport(workspaceID string, records map[collection][]map[string]any) (model.Export, error) {
	export := model.Export{
		Version:     2,
		WorkspaceID: workspaceID,
		Issues:      []model.Issue{},
		Relations:   []model.Relation{},
		Comments:    []model.Comment{},
		Labels:      []model.Label{},
		Events:      []model.IssueEvent{},
	}
	for _, rec := range records[collIssues] {
		issue, err := buildIssue(rec)
		if err != nil {
			return model.Export{}, err
		}
		export.Issues = append(export.Issues, issue)
	}
	for _, rec := range records[collRelations] {
		export.Relations = append(export.Relations, model.Relation{
			SrcID:     cellString(rec["src_id"]),
			DstID:     cellString(rec["dst_id"]),
			Type:      model.RelationType(cellString(rec["type"])), // [LAW:single-enforcer] exception: lifeboat salvage conserves bytes; Doctor re-checks
			CreatedAt: cellTime(rec["created_at"]),
			CreatedBy: cellString(rec["created_by"]),
		})
	}
	for _, rec := range records[collComments] {
		export.Comments = append(export.Comments, model.Comment{
			ID:        cellString(rec["id"]),
			IssueID:   cellString(rec["issue_id"]),
			Body:      cellString(rec["body"]),
			CreatedAt: cellTime(rec["created_at"]),
			CreatedBy: cellString(rec["created_by"]),
		})
	}
	for _, rec := range records[collLabels] {
		export.Labels = append(export.Labels, model.Label{
			IssueID:   cellString(rec["issue_id"]),
			Name:      cellString(rec["name"]),
			CreatedAt: cellTime(rec["created_at"]),
			CreatedBy: cellString(rec["created_by"]),
		})
	}

	// Build events, then graft change rows onto them by event_id. Index by id so
	// the nesting is a lookup, not a scan per change.
	events := make([]model.IssueEvent, 0, len(records[collEvents]))
	byID := map[string]int{}
	for _, rec := range records[collEvents] {
		ev := model.IssueEvent{
			ID:        cellString(rec["id"]),
			IssueID:   cellString(rec["issue_id"]),
			Action:    cellString(rec["action"]),
			Reason:    cellString(rec["reason"]),
			Actor:     cellString(rec["actor"]),
			CreatedAt: cellTime(rec["created_at"]),
			Changes:   []model.FieldChange{},
		}
		byID[ev.ID] = len(events)
		events = append(events, ev)
	}
	for _, rec := range records[collEventChanges] {
		eventID := cellString(rec["event_id"])
		idx, ok := byID[eventID]
		if !ok {
			return model.Export{}, fmt.Errorf("event change references unknown event_id %q", eventID)
		}
		events[idx].Changes = append(events[idx].Changes, model.FieldChange{
			Field: cellString(rec["field"]),
			From:  cellString(rec["from"]),
			To:    cellString(rec["to"]),
		})
	}
	export.Events = events
	return export, nil
}

// buildIssue assembles one hydrated model.Issue from its mapped record. The
// status/assignee/closed_at fields fold into the lifecycle through the same
// model.HydrateRow boundary every write path uses: a leaf carries its status
// value, a container (status NULL) hydrates as an empty AllOf — its derived state
// is recomputed from restored parent-child relations and never written, so empty
// members lose nothing durable. [LAW:single-enforcer]
func buildIssue(rec map[string]any) (model.Issue, error) {
	issue := model.Issue{
		ID:          cellString(rec["id"]),
		Title:       cellString(rec["title"]),
		Description: cellString(rec["description"]),
		Prompt:      cellString(rec["prompt"]),
		Priority:    cellInt(rec["priority"]),
		IssueType:   cellString(rec["issue_type"]),
		Topic:       cellString(rec["topic"]),
		Rank:        cellString(rec["rank"]),
		CreatedAt:   cellTime(rec["created_at"]),
		UpdatedAt:   cellTime(rec["updated_at"]),
		ArchivedAt:  cellTimePtr(rec["archived_at"]),
		DeletedAt:   cellTimePtr(rec["deleted_at"]),
	}
	view := model.StatusView{}
	if !model.IsContainerType(issue.IssueType) {
		view.Value = model.DefaultOpen(cellString(rec["status"]))
		view.Assignee = cellString(rec["assignee"])
		view.ClosedAt = cellTimePtr(rec["closed_at"])
	}
	return model.HydrateRow(issue, view, nil)
}

// applyTransform converts one source cell. [LAW:types-are-the-program] Each
// transform is total over its input including SQL NULL (nil): the structural
// transforms preserve NULL (the NULL-vs-"" distinction is load-bearing — epics
// carry NULL status), while the event canonicalizers coerce it on purpose to
// match recordEvent's live-write convention.
func applyTransform(t Transform, cell any) (any, error) {
	switch t {
	case TransformIdentity:
		return cell, nil
	case TransformTimestamp:
		if cell == nil {
			return nil, nil
		}
		s, ok := cell.(string)
		if !ok {
			return nil, fmt.Errorf("%s requires a string cell, got %T", t, cell)
		}
		// [LAW:no-silent-fallbacks] A non-NULL value that does not parse is a
		// corrupt dump, not an absent timestamp; surface it (Apply wraps this with
		// the source table and column) rather than zeroing the field and quietly
		// losing recovery data.
		ts, err := parseTimestamp(s)
		if err != nil {
			return nil, err
		}
		return ts, nil
	case TransformLegacyStatus:
		ns, err := cellNullString(cell)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		// Reuse the reconcile's canonicalizer so "what a legacy status maps to"
		// has one definition. [LAW:one-source-of-truth] NULL canonicalizes to NULL.
		return nullableSQLString(canonicalLegacyStatus(ns)), nil
	case TransformEventAction:
		ns, err := cellNullString(cell)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		return canonicalEventAction(ns), nil
	case TransformEventReason:
		ns, err := cellNullString(cell)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		return canonicalEventReason(ns), nil
	case TransformEventActor:
		ns, err := cellNullString(cell)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		return canonicalEventActor(ns), nil
	default:
		return nil, fmt.Errorf("unknown transform %q", t)
	}
}

// cellNullString converts a source cell to a sql.NullString for the
// canonicalizing transforms: SQL NULL → invalid, a string → valid. A non-string
// non-NULL cell in a column these transforms touch is a corrupt dump, surfaced
// rather than coerced.
func cellNullString(cell any) (sql.NullString, error) {
	switch v := cell.(type) {
	case nil:
		return sql.NullString{}, nil
	case string:
		return sql.NullString{Valid: true, String: v}, nil
	default:
		return sql.NullString{}, fmt.Errorf("expected a string or NULL cell, got %T", cell)
	}
}

// parseTimestamp parses a stored timestamp string. The store writes RFC3339Nano;
// the looser RFC3339 covers the second-precision values legacy (pre-goose)
// workspaces carry.
func parseTimestamp(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q", s)
}

// cellString renders a source cell as a domain string: SQL NULL and missing
// fields become "" (the write path re-derives NULLs from emptiness where the
// schema is nullable). dumpTable already normalized []byte text to string.
func cellString(cell any) string {
	switch v := cell.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func cellInt(cell any) int {
	switch v := cell.(type) {
	case nil:
		return 0
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case uint64:
		return int(v)
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	default:
		return 0
	}
}

// cellTime / cellTimePtr read a timestamp the TransformTimestamp fold already
// parsed into a time.Time (SQL NULL stays nil). The parse — and its failure —
// happened at the fold where the source table and column are known; here the
// value is trusted.
func cellTime(cell any) time.Time {
	if t, ok := cell.(time.Time); ok {
		return t
	}
	return time.Time{}
}

func cellTimePtr(cell any) *time.Time {
	if t, ok := cell.(time.Time); ok {
		return &t
	}
	return nil
}
