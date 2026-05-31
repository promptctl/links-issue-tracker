package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model"
)

// ShapeMapping is the seam between reasoning (an LLM, or a deterministic
// mapper) and mechanism (the applier). It is the declarative artifact that
// says, for one RawDump, how every source column becomes domain data.
//
// [LAW:types-are-the-program] A ShapeMapping is a TOTAL disposition over the
// source columns: each column is either mapped onto a domain field or dropped
// with a recorded provenance. Totality is not a structural property of the map
// (a producer supplies it as free data and can omit a key), so it is enforced
// once at the Validate trust boundary; past that boundary the applier assumes
// it. "Silently lost a column" is therefore not a state any applied mapping can
// be in — it is rejected before apply.
type ShapeMapping struct {
	Columns map[ColumnRef]Disposition
}

// ColumnRef names one source column positionally within the dump: a table and
// a column within it. It is the key of a ShapeMapping.
type ColumnRef struct {
	Table  string
	Column string
}

func (c ColumnRef) String() string { return c.Table + "." + c.Column }

// Disposition is the closed, two-variant classification of one source column.
// [LAW:types-are-the-program] A column is EITHER mapped to a domain field with
// an explicit value transform, OR dropped with a provenance — there is no third
// state and no "unset". The sealed interface (only this package can implement
// it) makes "mapped-and-dropped" and "neither" unrepresentable, so the applier
// never defends against them.
type Disposition interface{ isDisposition() }

// MappedTo carries a source cell onto one domain field. Target must resolve in
// targetRegistry, which is checked at the Validate boundary.
//
// [LAW:one-source-of-truth] The value conversion is NOT carried here: there is
// exactly one correct transform per target field (a timestamp field is always
// parsed, a status field is always canonicalized, everything else passes
// through), so the transform is a property of the target, looked up from the
// registry. Storing it alongside Target would be a second representation that
// could diverge — e.g. a timestamp target tagged "identity", which would slip
// an unparsed string past the timestamp check. Deriving it makes that
// unrepresentable.
type MappedTo struct {
	Target TargetKey
}

// Dropped records that a source column has no domain target, and why. The
// provenance is the discriminator that makes "notify only on drops" meaningful:
// an intended drop passes silently; an unexplained one is the surface-to-human
// case.
type Dropped struct {
	Provenance DropProvenance
	Reason     string
}

func (MappedTo) isDisposition() {}
func (Dropped) isDisposition()  {}

// DropProvenance discriminates a drop's justification.
type DropProvenance string

const (
	// DropIntended: the target baseline has no place for this column on
	// purpose — a migration removed it, or it is bookkeeping with no domain
	// representation by design. Mapping it to nothing is correct.
	DropIntended DropProvenance = "intended"
	// DropUnexplained: no target and no recorded reason. The genuine
	// surface-to-human case.
	DropUnexplained DropProvenance = "unexplained"
)

// Transform names a value conversion applied to a source cell as it lands on a
// domain field. The set is closed and small; a known shape adds a transform
// only when a real historical value vocabulary requires one.
type Transform string

const (
	TransformIdentity     Transform = "identity"
	TransformLegacyStatus Transform = "legacy_status_value"
	TransformTimestamp    Transform = "timestamp"
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
	coll      collection
	field     string
	transform Transform
}

// targetRegistry is the closed set of legal mapping targets: the
// version-independent domain contract (model.Export's collections), enumerated
// once. [LAW:one-source-of-truth] Valid targets live here; Validate consults
// this set and the applier dispatches on it, so neither invents its own idea of
// "what a field is".
//
// event_changes.event_id is the one join-only target: the applier consumes it
// to nest a change under its event; it is not a model.FieldChange field.
var targetRegistry = buildTargetRegistry()

func buildTargetRegistry() map[TargetKey]targetField {
	reg := map[TargetKey]targetField{}
	add := func(c collection, t Transform, fields ...string) {
		for _, f := range fields {
			reg[TargetKey(string(c)+"."+f)] = targetField{coll: c, field: f, transform: t}
		}
	}
	// The transform is fixed per field: timestamps are parsed, status is
	// canonicalized (idempotent on canonical values), everything else passes
	// through. Priority is identity here; the out-of-range clamp lives at the
	// import boundary. [LAW:single-enforcer]
	add(collIssues, TransformIdentity, "id", "title", "description", "prompt", "assignee", "priority", "issue_type", "topic", "rank")
	add(collIssues, TransformTimestamp, "closed_at", "created_at", "updated_at", "archived_at", "deleted_at")
	add(collIssues, TransformLegacyStatus, "status")
	add(collRelations, TransformIdentity, "src_id", "dst_id", "type", "created_by")
	add(collRelations, TransformTimestamp, "created_at")
	add(collComments, TransformIdentity, "id", "issue_id", "body", "created_by")
	add(collComments, TransformTimestamp, "created_at")
	add(collLabels, TransformIdentity, "issue_id", "name", "created_by")
	add(collLabels, TransformTimestamp, "created_at")
	add(collEvents, TransformIdentity, "id", "issue_id", "action", "reason", "actor")
	add(collEvents, TransformTimestamp, "created_at")
	add(collEventChanges, TransformIdentity, "event_id", "field", "from", "to")
	return reg
}

// Validate is the single well-formedness enforcer. [LAW:single-enforcer] It is
// the one trust boundary that decides whether a mapping (from an LLM or a
// deterministic mapper) may be applied. It rejects:
//   - a source column with no disposition, or a mapping key naming a column the
//     dump does not have (coverage totality);
//   - a disposition that is neither a well-formed MappedTo (target resolves) nor
//     a well-formed Dropped (known provenance);
//   - a source table whose mapped columns straddle more than one collection, or
//     two columns mapping to the same target field (which would silently
//     overwrite one another during assembly).
//
// After Validate succeeds, Apply treats the mapping as total and well-formed.
func Validate(dump RawDump, m ShapeMapping) error {
	dumpCols := map[ColumnRef]bool{}
	var unaccounted []string
	for _, table := range dump.Tables {
		for _, col := range table.Columns {
			ref := ColumnRef{Table: table.Name, Column: col}
			dumpCols[ref] = true
			if _, ok := m.Columns[ref]; !ok {
				unaccounted = append(unaccounted, ref.String())
			}
		}
	}
	if len(unaccounted) > 0 {
		sort.Strings(unaccounted)
		return fmt.Errorf("mapping is not total: %d source column(s) unaccounted for: %s",
			len(unaccounted), strings.Join(unaccounted, ", "))
	}

	var problems []string
	for ref, disp := range m.Columns {
		if !dumpCols[ref] {
			problems = append(problems, fmt.Sprintf("%s: mapping references a column the dump does not have", ref))
			continue
		}
		// [LAW:types-are-the-program] The boundary is total over the
		// disposition's shape, not just its presence: a column carrying a
		// well-formed MappedTo or Dropped is the only thing that survives. A nil
		// interface value or any other shape is rejected here rather than
		// silently treated as a drop by Apply.
		switch d := disp.(type) {
		case MappedTo:
			if _, ok := targetRegistry[d.Target]; !ok {
				problems = append(problems, fmt.Sprintf("%s: unknown target %q", ref, d.Target))
			}
		case Dropped:
			if d.Provenance != DropIntended && d.Provenance != DropUnexplained {
				problems = append(problems, fmt.Sprintf("%s: unknown drop provenance %q", ref, d.Provenance))
			}
		default:
			problems = append(problems, fmt.Sprintf("%s: disposition is neither MappedTo nor Dropped", ref))
		}
	}
	problems = append(problems, tableShapeProblems(dump, m)...)
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("mapping is malformed: %s", strings.Join(problems, "; "))
	}
	return nil
}

// tableShapeProblems reports the per-table structural faults that make a
// mapping unassemblable: a table feeding more than one collection, or two of
// its columns claiming the same target field. Both would otherwise surface as
// silent loss — the second cell overwriting the first, or an ambiguous record
// shape — so they are rejected before any row is read.
func tableShapeProblems(dump RawDump, m ShapeMapping) []string {
	var problems []string
	for _, table := range dump.Tables {
		coll := collection("")
		var collFound bool
		seenTarget := map[TargetKey]string{}
		for _, col := range table.Columns {
			mapped, ok := m.Columns[ColumnRef{Table: table.Name, Column: col}].(MappedTo)
			if !ok {
				continue
			}
			tf, ok := targetRegistry[mapped.Target]
			if !ok {
				continue // already reported as an unknown target above
			}
			if collFound && tf.coll != coll {
				problems = append(problems, fmt.Sprintf("table %q maps into multiple collections (%q and %q); one source table must map to one collection", table.Name, coll, tf.coll))
			}
			coll, collFound = tf.coll, true
			if prior, dup := seenTarget[mapped.Target]; dup {
				problems = append(problems, fmt.Sprintf("table %q: columns %q and %q both map to %q", table.Name, prior, col, mapped.Target))
			}
			seenTarget[mapped.Target] = col
		}
	}
	return problems
}

// Apply is the pure applier: it folds a RawDump through a validated mapping into
// a model.Export — the version-independent domain contract that
// ReplaceFromExport loads onto a fresh baseline. It assumes nothing about which
// producer wrote the mapping; deterministic and LLM mappers route through the
// identical fold.
//
// [LAW:dataflow-not-control-flow] The same fold runs for every column of every
// table; the mapping's values — target field and transform — are the only
// variability. There is no per-shape branch.
func Apply(dump RawDump, m ShapeMapping) (model.Export, error) {
	if err := Validate(dump, m); err != nil {
		return model.Export{}, err
	}
	records := map[collection][]map[string]any{}
	for _, table := range dump.Tables {
		coll, mappedCols := tableTarget(table, m)
		if len(mappedCols) == 0 {
			// Every column dropped: the table contributes no domain records.
			continue
		}
		for _, row := range table.Rows {
			rec := map[string]any{}
			for _, mc := range mappedCols {
				value, err := applyTransform(mc.transform, row[mc.index])
				if err != nil {
					return model.Export{}, fmt.Errorf("table %q column %q: %w", table.Name, table.Columns[mc.index], err)
				}
				rec[mc.field] = value
			}
			records[coll] = append(records[coll], rec)
		}
	}
	return assembleExport(dump.WorkspaceID, records)
}

// mappedColumn is a resolved MappedTo for one source column: where its cell
// lives in the row (index), the domain field it lands on, and the transform.
type mappedColumn struct {
	index     int
	field     string
	transform Transform
}

// tableTarget resolves the collection a source table feeds and its mapped
// columns, each with the field and transform read from the target registry. It
// is a pure resolver: Validate has already guaranteed the table feeds a single
// collection and that no two columns claim the same field, so there is nothing
// here to reject.
func tableTarget(table RawTable, m ShapeMapping) (collection, []mappedColumn) {
	var coll collection
	var cols []mappedColumn
	for i, name := range table.Columns {
		mapped, ok := m.Columns[ColumnRef{Table: table.Name, Column: name}].(MappedTo)
		if !ok {
			continue
		}
		tf := targetRegistry[mapped.Target]
		coll = tf.coll
		cols = append(cols, mappedColumn{index: i, field: tf.field, transform: tf.transform})
	}
	return coll, cols
}

// assembleExport turns the collected per-collection records into the typed
// domain structs of a model.Export. event_changes records are nested under
// their events by event_id.
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
			Type:      cellString(rec["type"]),
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

	// Build events, then graft change rows onto them by event_id. Index by id
	// so the nesting is a lookup, not a scan per change.
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
// value, a container (status NULL) hydrates as an empty AllOf — its derived
// state is recomputed from restored parent-child relations and never written,
// so empty members lose nothing durable. [LAW:single-enforcer]
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

// applyTransform converts one source cell. [LAW:types-are-the-program] SQL NULL
// (nil) always passes through unchanged: the NULL-vs-"" distinction is
// load-bearing (epics carry NULL status), and no transform may erase it.
func applyTransform(t Transform, cell any) (any, error) {
	if cell == nil {
		return nil, nil
	}
	switch t {
	case TransformIdentity:
		return cell, nil
	case TransformLegacyStatus:
		s, ok := cell.(string)
		if !ok {
			return nil, fmt.Errorf("%s requires a string cell, got %T", t, cell)
		}
		// Reuse the reconcile's canonicalizer so "what a legacy status maps to"
		// has one definition. [LAW:one-source-of-truth]
		return canonicalLegacyStatus(sql.NullString{Valid: true, String: s}).String, nil
	case TransformTimestamp:
		s, ok := cell.(string)
		if !ok {
			return nil, fmt.Errorf("%s requires a string cell, got %T", t, cell)
		}
		// [LAW:no-silent-fallbacks] A non-NULL value that does not parse is a
		// corrupt dump, not an absent timestamp; surface it (Apply wraps this
		// with the source table and column) rather than zeroing the field and
		// quietly losing recovery data.
		ts, err := parseTimestamp(s)
		if err != nil {
			return nil, err
		}
		return ts, nil
	default:
		return nil, fmt.Errorf("unknown transform %q", t)
	}
}

// parseTimestamp parses a stored timestamp string. The store writes
// RFC3339Nano; the looser RFC3339 covers the second-precision values legacy
// (pre-goose) workspaces carry.
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
