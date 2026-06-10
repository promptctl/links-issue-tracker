package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func ptr[T any](v T) *T { return &v }

// transitionActionCount counts lifecycle-transition events (as opposed to plain
// field-change events) among the supplied events. The container-update bug
// (links-update-container-ov6) was a phantom transition firing on a field-only
// update; this count is how the matrix makes "no transition happened" an
// explicit, machine-checkable assertion instead of an implicit gap.
func transitionActionCount(events []model.IssueEvent) int {
	// [LAW:one-source-of-truth] The transition-action vocabulary lives in the
	// model; sourcing it from the exported constants keeps this counter from
	// drifting if an action is renamed, rather than re-spelling the strings here.
	transitions := map[string]bool{
		string(model.ActionStart):  true,
		string(model.ActionDone):   true,
		string(model.ActionClose):  true,
		string(model.ActionReopen): true,
	}
	n := 0
	for _, e := range events {
		if transitions[e.Action] {
			n++
		}
	}
	return n
}

// TestApplyUpdateIssueTypeFlagMatrix asserts a documented outcome for every
// model.ValidIssueTypes × meaningful-flag-combination cell of the unified update
// path. The matrix exists to close the implicit gap that let
// links-update-container-ov6 ship: no test covered (epic, field-only), so a
// phantom status transition on containers went unnoticed.
//
// [LAW:single-enforcer] The cells drive Store.ApplyUpdate — the one execution
// path for `lit update` — rather than reimplementing the transition decision, so
// the assertions cannot drift from the code they guard.
//
// [LAW:types-are-the-program] The accept/reject outcome of a cell is not hand
// enumerated; it is the theorem the type already encodes — a cell is rejected
// iff it asks a container to transition. wantErr is computed from
// (IsContainerType × combo-carries-a-target-status), and the test proves the
// implementation agrees. Field writes succeed on every type; leaf transitions
// succeed for every target; only container transitions are refused.
func TestApplyUpdateIssueTypeFlagMatrix(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)

	const (
		initialTitle       = "Initial title"
		initialDescription = "Initial body"
		initialPriority    = model.PriorityNormal
	)

	combos := []struct {
		name string
		in   ApplyUpdateInput
	}{
		{name: "no_flags", in: ApplyUpdateInput{}},
		{name: "title_only", in: ApplyUpdateInput{Fields: UpdateIssueInput{Title: ptr("Renamed")}}},
		{name: "description_only", in: ApplyUpdateInput{Fields: UpdateIssueInput{Description: ptr("Rewritten body")}}},
		{name: "priority_only", in: ApplyUpdateInput{Fields: UpdateIssueInput{Priority: ptr(model.PriorityUrgent)}}},
		{name: "labels_only", in: ApplyUpdateInput{Fields: UpdateIssueInput{Labels: ptr([]string{"alpha"})}}},
		{name: "status_open", in: ApplyUpdateInput{TargetStatus: "open"}},
		{name: "status_in_progress", in: ApplyUpdateInput{TargetStatus: "in_progress"}},
		{name: "status_closed", in: ApplyUpdateInput{TargetStatus: "closed"}},
		{name: "title_and_status_open", in: ApplyUpdateInput{Fields: UpdateIssueInput{Title: ptr("Renamed")}, TargetStatus: "open"}},
	}

	for _, issueType := range model.ValidIssueTypes {
		for _, combo := range combos {
			t.Run(issueType+"/"+combo.name, func(t *testing.T) {
				created, err := st.CreateIssue(ctx, CreateIssueInput{
					Prefix:      "test",
					Title:       initialTitle,
					Description: initialDescription,
					Topic:       "update-matrix",
					IssueType:   issueType,
					Priority:    initialPriority,
				})
				if err != nil {
					t.Fatalf("CreateIssue(%s) error = %v", issueType, err)
				}

				before, err := st.GetIssueDetail(ctx, created.ID)
				if err != nil {
					t.Fatalf("GetIssueDetail(before) error = %v", err)
				}

				in := combo.in
				in.TransitionBy = "tester"
				in.TransitionAssignee = "tester"
				in.Fields.By = "tester"

				carriesTransition := in.TargetStatus != ""
				// [LAW:types-are-the-program] The single discriminator: a
				// container has no own status, so any transition request is
				// refused; every other cell is accepted.
				wantErr := model.IsContainerType(issueType) && carriesTransition

				updated, err := st.ApplyUpdate(ctx, created.ID, in)

				after, detailErr := st.GetIssueDetail(ctx, created.ID)
				if detailErr != nil {
					t.Fatalf("GetIssueDetail(after) error = %v", detailErr)
				}
				added := after.Events[len(before.Events):]

				if wantErr {
					if err == nil {
						t.Fatalf("ApplyUpdate(%s, %s) error = nil, want container transition refusal", issueType, combo.name)
					}
					var containerErr model.ContainerActionError
					if !errors.As(err, &containerErr) {
						t.Fatalf("ApplyUpdate(%s, %s) error = %q, want model.ContainerActionError container refusal", issueType, combo.name, err)
					}
					// The transition is attempted before any field write, so a
					// rejected container update must leave the issue wholly
					// untouched — no partial field write of ANY field, and no
					// recorded event of any kind (transition or field-change).
					if after.Issue.Title != initialTitle {
						t.Fatalf("ApplyUpdate(%s, %s) wrote title %q on a rejected update; want unchanged %q", issueType, combo.name, after.Issue.Title, initialTitle)
					}
					if after.Issue.Description != initialDescription {
						t.Fatalf("ApplyUpdate(%s, %s) wrote description %q on a rejected update; want unchanged %q", issueType, combo.name, after.Issue.Description, initialDescription)
					}
					if after.Issue.Priority != initialPriority {
						t.Fatalf("ApplyUpdate(%s, %s) wrote priority %d on a rejected update; want unchanged %d", issueType, combo.name, after.Issue.Priority, initialPriority)
					}
					if len(after.Issue.Labels) != 0 {
						t.Fatalf("ApplyUpdate(%s, %s) wrote labels %v on a rejected update; want none", issueType, combo.name, after.Issue.Labels)
					}
					if len(added) != 0 {
						t.Fatalf("ApplyUpdate(%s, %s) recorded %d events on a rejected update, want 0: %#v", issueType, combo.name, len(added), added)
					}
					return
				}

				if err != nil {
					t.Fatalf("ApplyUpdate(%s, %s) error = %v, want success", issueType, combo.name, err)
				}

				if in.Fields.Title != nil && updated.Title != *in.Fields.Title {
					t.Fatalf("ApplyUpdate(%s, %s) title = %q, want %q", issueType, combo.name, updated.Title, *in.Fields.Title)
				}
				if in.Fields.Description != nil && updated.Description != *in.Fields.Description {
					t.Fatalf("ApplyUpdate(%s, %s) description = %q, want %q", issueType, combo.name, updated.Description, *in.Fields.Description)
				}
				if in.Fields.Priority != nil && updated.Priority != *in.Fields.Priority {
					t.Fatalf("ApplyUpdate(%s, %s) priority = %d, want %d", issueType, combo.name, updated.Priority, *in.Fields.Priority)
				}
				if in.Fields.Labels != nil && strings.Join(updated.Labels, ",") != strings.Join(*in.Fields.Labels, ",") {
					t.Fatalf("ApplyUpdate(%s, %s) labels = %v, want %v", issueType, combo.name, updated.Labels, *in.Fields.Labels)
				}
				if carriesTransition && updated.State() != model.DefaultOpen(in.TargetStatus) {
					t.Fatalf("ApplyUpdate(%s, %s) state = %q, want %q", issueType, combo.name, updated.State(), model.DefaultOpen(in.TargetStatus))
				}

				// The ov6 guard, made explicit: a field-only cell records zero
				// transition events on every type — most importantly on a
				// container, where the phantom transition once fired. A
				// same-state target with an unchanged assignee is the leaf's
				// documented no-op and likewise records nothing; only a
				// transition that mutates the row earns an event.
				wantTransitions := 0
				if carriesTransition && model.DefaultOpen(in.TargetStatus) != created.State() {
					wantTransitions = 1
				}
				if n := transitionActionCount(added); n != wantTransitions {
					t.Fatalf("ApplyUpdate(%s, %s) recorded %d transition events, want %d: %#v", issueType, combo.name, n, wantTransitions, added)
				}
			})
		}
	}
}
