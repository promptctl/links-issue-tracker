package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// BulkApply creates or updates every issue named by specs: a document
// without an ID creates (in dependency order, same as ImportTree); a
// document with an ID applies its set fields to that existing issue. On any
// failure mid-batch, BulkApply best-effort rolls back issues it created in
// this call (never issues it updated — an update has no "prior create" to
// unwind). Run `lit doctor` after a failed batch to detect orphans left if
// rollback itself failed. [LAW:single-enforcer] one boundary owns ID
// resolution, create/update dispatch, and topological create order.
func (s *Store) BulkApply(ctx context.Context, prefix, actor string, specs []storage.BulkIssueSpec) (storage.BulkApplyResult, error) {
	if err := storage.ValidateBulkSpecs(specs); err != nil {
		return storage.BulkApplyResult{}, err
	}
	localID := make([]string, len(specs))
	parent := make([]string, len(specs))
	dependsOn := make([][]string, len(specs))
	for i, spec := range specs {
		localID[i] = spec.LocalID
		parent[i] = spec.Parent
		dependsOn[i] = spec.BlockedBy
	}
	order, err := topoSortLocalGraph(localID, parent, dependsOn)
	if err != nil {
		return storage.BulkApplyResult{}, fmt.Errorf("bulk: %w", err)
	}

	result := storage.BulkApplyResult{}
	createdRealID := make([]string, len(specs))
	createdIDs := make([]string, 0, len(specs))
	localRealID := make(map[string]string, len(specs))

	for _, idx := range order {
		spec := specs[idx]
		if spec.ID != "" {
			change, err := bulkUpdateChange(spec, actor)
			if err != nil {
				return storage.BulkApplyResult{}, err
			}
			issue, err := s.Apply(ctx, spec.ID, change)
			if err != nil {
				leaked := s.rollbackCreatedIssues(ctx, createdIDs)
				return storage.BulkApplyResult{}, fmt.Errorf("bulk: update %q: %w (rollback leaked %d: %s)", spec.ID, err, len(leaked), strings.Join(leaked, ","))
			}
			result.Updated = append(result.Updated, issue.ID)
			continue
		}
		// validateBulkSpecs already required Title/Topic/IssueType present and
		// IssueType/Priority valid before the first create; parsing again here
		// is the same single gate producing the typed value.
		issueType, err := model.ParseIssueType(*spec.IssueType)
		if err != nil {
			return storage.BulkApplyResult{}, fmt.Errorf("bulk: doc %d: %w", idx, err)
		}
		priority := model.PriorityNormal
		if spec.Priority != nil {
			priority, err = model.ParsePriority(*spec.Priority)
			if err != nil {
				return storage.BulkApplyResult{}, fmt.Errorf("bulk: doc %d: %w", idx, err)
			}
		}
		issue, err := s.CreateIssue(ctx, storage.CreateIssueInput{
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
			// Placement is left at its zero value (RankBottom), matching
			// ImportTree — both are `lit import --path <file>`, so a create
			// lands the same place in the ranked order regardless of which
			// format the file is, and a batch keeps its file order with no
			// flag to remember. [LAW:one-source-of-truth]
			Prefix: prefix,
		})
		if err != nil {
			leaked := s.rollbackCreatedIssues(ctx, createdIDs)
			return storage.BulkApplyResult{}, fmt.Errorf("bulk: create doc %d: %w (rollback leaked %d: %s)", idx, err, len(leaked), strings.Join(leaked, ","))
		}
		createdRealID[idx] = issue.ID
		createdIDs = append(createdIDs, issue.ID)
		if spec.LocalID != "" {
			localRealID[spec.LocalID] = issue.ID
		}
		result.Created = append(result.Created, storage.NewIDMapping(spec.LocalID, issue.ID))
	}
	for i, spec := range specs {
		if spec.ID != "" {
			continue
		}
		for _, dep := range spec.BlockedBy {
			srcID := createdRealID[i]
			dstID := resolveBulkRef(dep, localRealID)
			if _, err := s.AddRelation(ctx, storage.AddRelationInput{SrcID: srcID, DstID: dstID, Type: model.RelBlockedBy, CreatedBy: "links"}); err != nil {
				leaked := s.rollbackCreatedIssues(ctx, createdIDs)
				return storage.BulkApplyResult{}, fmt.Errorf("bulk: blocked_by doc %d -> %q: %w (rollback leaked %d: %s)", i, dep, err, len(leaked), strings.Join(leaked, ","))
			}
		}
	}
	return result, nil
}

// resolveBulkRef resolves a parent/blocked_by reference against this batch's
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
func bulkUpdateChange(spec storage.BulkIssueSpec, actor string) (storage.Change, error) {
	fields := storage.UpdateIssueInput{Reason: strings.TrimSpace(spec.Reason)}
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
			return storage.Change{}, fmt.Errorf("bulk: update %q: %w", spec.ID, err)
		}
		fields.IssueType = &v
	}
	if spec.Priority != nil {
		v, err := model.ParsePriority(*spec.Priority)
		if err != nil {
			return storage.Change{}, fmt.Errorf("bulk: update %q: %w", spec.ID, err)
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
	return storage.Change{Actor: actor, Fields: fields}, nil
}

func derefOr[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}
