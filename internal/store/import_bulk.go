package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// BulkIssueSpec is a single YAML document in a bulk-input file. ID is the
// selector: present, it names an existing issue and the document is an
// update patch of only the fields it sets (the same fields UpdateIssueInput
// exposes, one YAML key per field). Absent, the document creates a new
// issue and behaves like ImportTreeSpec's flat form — LocalID, Parent, and
// DependsOn wire the new issue against siblings created in the same file (by
// LocalID) or against pre-existing issues (by real ID); title/topic/type are
// required as they are for any create. [LAW:types-are-the-program] Pointer
// fields are exactly the update-patch fields: nil means "leave unchanged,"
// a set pointer means "write this value" — the same distinction
// UpdateIssueInput's pointers carry, so a create-doc's omitted optional
// field and an update-doc's omitted field defer to the same "unspecified"
// representation instead of each needing its own convention.
type BulkIssueSpec struct {
	LocalID     string    `yaml:"local_id,omitempty"`
	ID          string    `yaml:"id,omitempty"`
	Title       *string   `yaml:"title,omitempty"`
	Description *string   `yaml:"description,omitempty"`
	Prompt      *string   `yaml:"prompt,omitempty"`
	IssueType   *string   `yaml:"type,omitempty"`
	Topic       *string   `yaml:"topic,omitempty"`
	Priority    *int      `yaml:"priority,omitempty"`
	Assignee    *string   `yaml:"assignee,omitempty"`
	Labels      *[]string `yaml:"labels,omitempty"`
	Lane        *string   `yaml:"lane,omitempty"`
	Parent      string    `yaml:"parent,omitempty"`
	DependsOn   []string  `yaml:"depends_on,omitempty"`
	// Reason is optional free text recorded on an update's field-change
	// event; it does not apply to a create (there is no prior state to
	// annotate a change against).
	Reason string `yaml:"reason,omitempty"`
}

// BulkApplyResult reports what a successful BulkApply did. Created maps each
// create document's own reference — its LocalID if it set one, otherwise its
// new real ID — to the real ID it was created under, so every create is
// nameable in the report even when the file never gave it a LocalID.
// Updated lists the real IDs of every updated issue, in the order they were
// applied.
type BulkApplyResult struct {
	Created map[string]string
	Updated []string
}

// ParseBulkSpecs is the deserialization trust boundary for bulk-input files:
// raw YAML bytes in, one spec per document out. It rejects any field the
// spec schema does not name, so a typo'd key fails loudly here instead of
// silently doing nothing. [LAW:single-enforcer] [LAW:no-silent-failure]
func ParseBulkSpecs(data []byte) ([]BulkIssueSpec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var specs []BulkIssueSpec
	for {
		var spec BulkIssueSpec
		if err := dec.Decode(&spec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("bulk: parse spec: %w", err)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// BulkApply creates or updates every issue named by specs: a document
// without an ID creates (in dependency order, same as ImportTree); a
// document with an ID applies its set fields to that existing issue. On any
// failure mid-batch, BulkApply best-effort rolls back issues it created in
// this call (never issues it updated — an update has no "prior create" to
// unwind). Run `lit doctor` after a failed batch to detect orphans left if
// rollback itself failed. [LAW:single-enforcer] one boundary owns ID
// resolution, create/update dispatch, and topological create order.
func (s *Store) BulkApply(ctx context.Context, prefix, actor string, specs []BulkIssueSpec) (BulkApplyResult, error) {
	if err := validateBulkSpecs(specs); err != nil {
		return BulkApplyResult{}, err
	}
	localID := make([]string, len(specs))
	parent := make([]string, len(specs))
	dependsOn := make([][]string, len(specs))
	for i, spec := range specs {
		localID[i] = spec.LocalID
		parent[i] = spec.Parent
		dependsOn[i] = spec.DependsOn
	}
	order, err := topoSortLocalGraph(localID, parent, dependsOn)
	if err != nil {
		return BulkApplyResult{}, fmt.Errorf("bulk: %w", err)
	}

	result := BulkApplyResult{Created: map[string]string{}}
	createdRealID := make([]string, len(specs))
	createdIDs := make([]string, 0, len(specs))
	localRealID := make(map[string]string, len(specs))

	for _, idx := range order {
		spec := specs[idx]
		if spec.ID != "" {
			change, err := bulkUpdateChange(spec, actor)
			if err != nil {
				return BulkApplyResult{}, err
			}
			issue, err := s.Apply(ctx, spec.ID, change)
			if err != nil {
				leaked := s.rollbackCreatedIssues(ctx, createdIDs)
				return BulkApplyResult{}, fmt.Errorf("bulk: update %q: %w (rollback leaked %d: %s)", spec.ID, err, len(leaked), strings.Join(leaked, ","))
			}
			result.Updated = append(result.Updated, issue.ID)
			continue
		}
		// validateBulkSpecs already required Title/Topic/IssueType present and
		// IssueType/Priority valid before the first create; parsing again here
		// is the same single gate producing the typed value.
		issueType, err := model.ParseIssueType(*spec.IssueType)
		if err != nil {
			return BulkApplyResult{}, fmt.Errorf("bulk: doc %d: %w", idx, err)
		}
		priority := model.PriorityNormal
		if spec.Priority != nil {
			priority, err = model.ParsePriority(*spec.Priority)
			if err != nil {
				return BulkApplyResult{}, fmt.Errorf("bulk: doc %d: %w", idx, err)
			}
		}
		issue, err := s.CreateIssue(ctx, CreateIssueInput{
			Title:       strings.TrimSpace(*spec.Title),
			Description: derefOr(spec.Description, ""),
			Prompt:      derefOr(spec.Prompt, ""),
			IssueType:   issueType,
			Topic:       strings.TrimSpace(*spec.Topic),
			ParentID:    resolveBulkRef(spec.Parent, localRealID),
			Priority:    priority,
			Assignee:    derefOr(spec.Assignee, ""),
			Lane:        derefOr(spec.Lane, ""),
			Labels:      derefOr(spec.Labels, nil),
			Placement:   RankBottom,
			Prefix:      prefix,
		})
		if err != nil {
			leaked := s.rollbackCreatedIssues(ctx, createdIDs)
			return BulkApplyResult{}, fmt.Errorf("bulk: create doc %d: %w (rollback leaked %d: %s)", idx, err, len(leaked), strings.Join(leaked, ","))
		}
		createdRealID[idx] = issue.ID
		createdIDs = append(createdIDs, issue.ID)
		if spec.LocalID != "" {
			localRealID[spec.LocalID] = issue.ID
			result.Created[spec.LocalID] = issue.ID
		} else {
			result.Created[issue.ID] = issue.ID
		}
	}
	for i, spec := range specs {
		if spec.ID != "" {
			continue
		}
		for _, dep := range spec.DependsOn {
			srcID := createdRealID[i]
			dstID := resolveBulkRef(dep, localRealID)
			// blocks convention in the store: src is dependent, dst is
			// dependency. spec says "srcID depends_on dstID", so src is dependent.
			if _, err := s.AddRelation(ctx, AddRelationInput{SrcID: srcID, DstID: dstID, Type: "blocks", CreatedBy: "links"}); err != nil {
				leaked := s.rollbackCreatedIssues(ctx, createdIDs)
				return BulkApplyResult{}, fmt.Errorf("bulk: depends_on doc %d -> %q: %w (rollback leaked %d: %s)", i, dep, err, len(leaked), strings.Join(leaked, ","))
			}
		}
	}
	return result, nil
}

// resolveBulkRef resolves a parent/depends_on reference against this batch's
// LocalID -> real-ID map; a reference that matches nothing in the batch
// passes through unchanged as a real, pre-existing issue ID for CreateIssue
// or AddRelation to validate. [LAW:dataflow-not-control-flow] one
// unconditional lookup-with-fallback, not a branch on "is this local or
// external."
func resolveBulkRef(ref string, localRealID map[string]string) string {
	if real, ok := localRealID[ref]; ok {
		return real
	}
	return ref
}

// bulkUpdateChange builds the field-patch Change for an update document: one
// pointer copy per set field, mirroring `lit update`'s per-flag assembly.
// validateBulkSpecs has already confirmed IssueType/Priority parse and that
// at least one field is set; the parse errors are still propagated rather
// than discarded, the same trust-but-verify convention BulkApply's create
// branch and ImportTree both use for the same already-validated fields.
// [LAW:no-silent-failure]
func bulkUpdateChange(spec BulkIssueSpec, actor string) (Change, error) {
	fields := UpdateIssueInput{Reason: strings.TrimSpace(spec.Reason)}
	if spec.Title != nil {
		v := strings.TrimSpace(*spec.Title)
		fields.Title = &v
	}
	if spec.Description != nil {
		v := strings.TrimSpace(*spec.Description)
		fields.Description = &v
	}
	if spec.Prompt != nil {
		v := strings.TrimSpace(*spec.Prompt)
		fields.Prompt = &v
	}
	if spec.IssueType != nil {
		v, err := model.ParseIssueType(*spec.IssueType)
		if err != nil {
			return Change{}, fmt.Errorf("bulk: update %q: %w", spec.ID, err)
		}
		fields.IssueType = &v
	}
	if spec.Priority != nil {
		v, err := model.ParsePriority(*spec.Priority)
		if err != nil {
			return Change{}, fmt.Errorf("bulk: update %q: %w", spec.ID, err)
		}
		fields.Priority = &v
	}
	if spec.Assignee != nil {
		v := strings.TrimSpace(*spec.Assignee)
		fields.Assignee = &v
	}
	if spec.Lane != nil {
		v := strings.TrimSpace(*spec.Lane)
		fields.Lane = &v
	}
	if spec.Labels != nil {
		v := *spec.Labels
		fields.Labels = &v
	}
	return Change{Actor: actor, Fields: fields}, nil
}

func derefOr[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}

// validateBulkSpecs enumerates the full accept/reject shape of a bulk-input
// file before any document is applied, so a batch either wholly makes sense
// or fails before touching the store:
//
//	MUST ACCEPT
//	  - id set; only update-eligible fields set (title/description/prompt/
//	    type/priority/assignee/labels/lane, at least one); no local_id,
//	    topic, parent, or depends_on.
//	  - id unset; title, topic, and type set (type valid); priority unset
//	    (defaults) or valid; parent/depends_on/local_id set freely.
//	  - two different documents naming the same local_id-or-external parent,
//	    or the same depends_on target — sharing a reference is not sharing
//	    an id.
//
//	MUST REJECT
//	  - empty file (zero documents).
//	  - id or local_id with surrounding whitespace.
//	  - id set and local_id also set (local_id is create-only).
//	  - id set and topic set (topic is immutable; update cannot change it).
//	  - id set and parent set (reparenting is `lit parent set`'s job).
//	  - id set and depends_on set (dependency wiring is `lit dep add`'s job).
//	  - id set and reason set with no other field set (nothing to annotate).
//	  - id set but zero update-eligible fields set (matches `lit update`
//	    requiring at least one field flag).
//	  - id set twice across documents (each document owns one ticket).
//	  - id unset and title, topic, or type missing.
//	  - id unset and reason set (no field-change event to annotate).
//	  - local_id duplicated across two create documents.
//	  - a document's own local_id appearing in its own depends_on.
//	  - type or priority present but invalid, on either side.
//
// [LAW:types-are-the-program] the two branches below are exactly the two
// legal document shapes; nothing outside them is representable as valid.
func validateBulkSpecs(specs []BulkIssueSpec) error {
	if len(specs) == 0 {
		return errors.New("bulk: no issues in input")
	}
	seenID := make(map[string]struct{}, len(specs))
	seenLocal := make(map[string]struct{}, len(specs))
	for i, spec := range specs {
		if spec.ID != strings.TrimSpace(spec.ID) {
			return fmt.Errorf("bulk: doc %d id %q has surrounding whitespace", i, spec.ID)
		}
		if spec.LocalID != strings.TrimSpace(spec.LocalID) {
			return fmt.Errorf("bulk: doc %d local_id %q has surrounding whitespace", i, spec.LocalID)
		}
		if spec.Parent != strings.TrimSpace(spec.Parent) {
			return fmt.Errorf("bulk: doc %d parent %q has surrounding whitespace", i, spec.Parent)
		}
		for _, dep := range spec.DependsOn {
			if dep != strings.TrimSpace(dep) {
				return fmt.Errorf("bulk: doc %d depends_on entry %q has surrounding whitespace", i, dep)
			}
			if spec.LocalID != "" && dep == spec.LocalID {
				return fmt.Errorf("bulk: doc %d (local_id %q) cannot depend on itself", i, spec.LocalID)
			}
		}

		if spec.ID != "" {
			if err := validateBulkUpdateDoc(i, spec); err != nil {
				return err
			}
			if _, dup := seenID[spec.ID]; dup {
				return fmt.Errorf("bulk: duplicate id %q", spec.ID)
			}
			seenID[spec.ID] = struct{}{}
			continue
		}

		if err := validateBulkCreateDoc(i, spec); err != nil {
			return err
		}
		if spec.LocalID != "" {
			if _, dup := seenLocal[spec.LocalID]; dup {
				return fmt.Errorf("bulk: duplicate local_id %q", spec.LocalID)
			}
			seenLocal[spec.LocalID] = struct{}{}
		}
	}
	return nil
}

func validateBulkCreateDoc(i int, spec BulkIssueSpec) error {
	if spec.Title == nil || strings.TrimSpace(*spec.Title) == "" {
		return fmt.Errorf("bulk: doc %d missing title", i)
	}
	if spec.Topic == nil || strings.TrimSpace(*spec.Topic) == "" {
		return fmt.Errorf("bulk: doc %d missing topic", i)
	}
	if spec.IssueType == nil {
		return fmt.Errorf("bulk: doc %d missing type", i)
	}
	if _, err := model.ParseIssueType(*spec.IssueType); err != nil {
		return fmt.Errorf("bulk: doc %d has invalid type %q", i, *spec.IssueType)
	}
	if spec.Priority != nil {
		if _, err := model.ParsePriority(*spec.Priority); err != nil {
			return fmt.Errorf("bulk: doc %d has invalid priority %d", i, *spec.Priority)
		}
	}
	if spec.Reason != "" {
		return fmt.Errorf("bulk: doc %d sets reason without id (reason only applies to updates)", i)
	}
	return nil
}

func validateBulkUpdateDoc(i int, spec BulkIssueSpec) error {
	if spec.LocalID != "" {
		return fmt.Errorf("bulk: doc %d (id %q) sets local_id; local_id only applies to new tickets", i, spec.ID)
	}
	if spec.Topic != nil {
		return fmt.Errorf("bulk: doc %d (id %q) sets topic; topic is immutable and update cannot change it", i, spec.ID)
	}
	if spec.Parent != "" {
		return fmt.Errorf("bulk: doc %d (id %q) sets parent; reparent with `lit parent set` instead", i, spec.ID)
	}
	if len(spec.DependsOn) > 0 {
		return fmt.Errorf("bulk: doc %d (id %q) sets depends_on; wire dependencies with `lit dep add` instead", i, spec.ID)
	}
	if spec.IssueType != nil {
		if _, err := model.ParseIssueType(*spec.IssueType); err != nil {
			return fmt.Errorf("bulk: doc %d (id %q) has invalid type %q", i, spec.ID, *spec.IssueType)
		}
	}
	if spec.Priority != nil {
		if _, err := model.ParsePriority(*spec.Priority); err != nil {
			return fmt.Errorf("bulk: doc %d (id %q) has invalid priority %d", i, spec.ID, *spec.Priority)
		}
	}
	if !bulkUpdateHasField(spec) {
		return fmt.Errorf("bulk: doc %d (id %q) has no fields to update", i, spec.ID)
	}
	return nil
}

func bulkUpdateHasField(spec BulkIssueSpec) bool {
	return spec.Title != nil || spec.Description != nil || spec.Prompt != nil ||
		spec.IssueType != nil || spec.Priority != nil || spec.Assignee != nil ||
		spec.Labels != nil || spec.Lane != nil
}
