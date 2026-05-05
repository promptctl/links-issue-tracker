package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func hydratedIssue(t *testing.T, issue Issue, status State) Issue {
	t.Helper()
	hydrated, err := HydrateOwnedStatus(issue, StatusView{Value: status})
	if err != nil {
		t.Fatalf("HydrateOwnedStatus() error = %v", err)
	}
	return hydrated
}

func TestApplyRefusesContainerEvenWhenMembersAreActionable(t *testing.T) {
	childA := hydratedIssue(t, Issue{ID: "a", IssueType: "task"}, StateOpen)
	childB := hydratedIssue(t, Issue{ID: "b", IssueType: "task"}, StateOpen)
	container, err := HydrateAllOf(Issue{ID: "epic", IssueType: "epic"}, []Issue{childA, childB})
	if err != nil {
		t.Fatalf("HydrateAllOf() error = %v", err)
	}
	_, err = container.Apply(ActionStart, "tester", "")
	if err == nil || err.Error() != "no start action available on this issue" {
		t.Fatalf("Apply(start) error = %v, want no start action available", err)
	}
}

func TestApplyIdempotentReportsAlreadyInState(t *testing.T) {
	tests := []struct {
		name    string
		state   State
		action  ActionName
		wantErr string
	}{
		{name: "start on in_progress", state: StateInProgress, action: ActionStart, wantErr: "issue is already in progress"},
		{name: "done on closed", state: StateClosed, action: ActionDone, wantErr: "issue is already closed"},
		{name: "close on closed", state: StateClosed, action: ActionClose, wantErr: "issue is already closed"},
		{name: "reopen on open", state: StateOpen, action: ActionReopen, wantErr: "issue is already open"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leaf := hydratedIssue(t, Issue{ID: "leaf", IssueType: "task"}, tt.state)
			_, err := leaf.Apply(tt.action, "tester", "")
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("Apply(%s on %s) error = %v, want %q", tt.action, tt.state, err, tt.wantErr)
			}
		})
	}
}

func TestApplyNonIdempotentInvalidTransitionKeepsLegacyMessage(t *testing.T) {
	// reopen on in_progress is invalid but NOT idempotent — target state of
	// reopen is Open, not InProgress — so the legacy "no X action available"
	// message is preserved. Same for done on open and start on closed.
	tests := []struct {
		name   string
		state  State
		action ActionName
	}{
		{name: "reopen on in_progress", state: StateInProgress, action: ActionReopen},
		{name: "done on open", state: StateOpen, action: ActionDone},
		{name: "start on closed", state: StateClosed, action: ActionStart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leaf := hydratedIssue(t, Issue{ID: "leaf", IssueType: "task"}, tt.state)
			_, err := leaf.Apply(tt.action, "tester", "")
			want := "no " + string(tt.action) + " action available on this issue"
			if err == nil || err.Error() != want {
				t.Fatalf("Apply(%s on %s) error = %v, want %q", tt.action, tt.state, err, want)
			}
		})
	}
}

func TestIssueJSONRoundTripEpicRequiresStoreHydration(t *testing.T) {
	epic, err := HydrateAllOf(Issue{
		ID:        "epic-1",
		Title:     "Container",
		IssueType: "epic",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatalf("HydrateAllOf() error = %v", err)
	}
	data, err := json.Marshal(epic)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded Issue
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded.IssueType != "epic" {
		t.Fatalf("IssueType = %q, want epic", decoded.IssueType)
	}
	if !decoded.pendingHydration {
		t.Fatalf("decoded epic pendingHydration = false, want true")
	}
	if decoded.Capabilities().Status != nil {
		t.Fatalf("Capabilities().Status = %#v, want nil", decoded.Capabilities().Status)
	}
	if decoded.State() != "" || decoded.Progress() != (Progress{}) {
		t.Fatalf("decoded epic state/progress = %q/%#v, want zero values before store hydration", decoded.State(), decoded.Progress())
	}
	if _, err := json.Marshal(decoded); err == nil || !strings.Contains(err.Error(), "requires store hydration") {
		t.Fatalf("Marshal(decoded epic) error = %v, want hydration error", err)
	}
}

func TestIssueJSONRoundTripLeafPreservesStatusFields(t *testing.T) {
	closedAt := time.Now().UTC()
	leaf, err := HydrateOwnedStatus(Issue{
		ID:        "task-1",
		Title:     "Leaf",
		IssueType: "task",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, StatusView{Value: StateClosed, Assignee: "dev", ClosedAt: &closedAt})
	if err != nil {
		t.Fatalf("HydrateOwnedStatus() error = %v", err)
	}
	data, err := json.Marshal(leaf)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded Issue
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded.StatusValue() != string(StateClosed) {
		t.Fatalf("StatusValue() = %q, want closed", decoded.StatusValue())
	}
	if decoded.AssigneeValue() != "dev" {
		t.Fatalf("AssigneeValue() = %q, want dev", decoded.AssigneeValue())
	}
	if decoded.ClosedAtValue() == nil || !decoded.ClosedAtValue().Equal(closedAt) {
		t.Fatalf("ClosedAtValue() = %#v, want %s", decoded.ClosedAtValue(), closedAt)
	}
}

func TestIssueJSONRoundTripPreservesPrompt(t *testing.T) {
	leaf, err := HydrateOwnedStatus(Issue{
		ID:        "task-1",
		Title:     "Leaf with prompt",
		IssueType: "task",
		Prompt:    "Run the renderer headless and assert no NaNs.",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, StatusView{Value: StateOpen})
	if err != nil {
		t.Fatalf("HydrateOwnedStatus() error = %v", err)
	}
	data, err := json.Marshal(leaf)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded Issue
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded.Prompt != leaf.Prompt {
		t.Fatalf("decoded.Prompt = %q, want %q", decoded.Prompt, leaf.Prompt)
	}

	// Empty prompt should be omitted from the JSON wire shape entirely.
	bare, err := HydrateOwnedStatus(Issue{
		ID:        "task-2",
		Title:     "Leaf without prompt",
		IssueType: "task",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, StatusView{Value: StateOpen})
	if err != nil {
		t.Fatalf("HydrateOwnedStatus(bare) error = %v", err)
	}
	bareData, err := json.Marshal(bare)
	if err != nil {
		t.Fatalf("Marshal(bare) error = %v", err)
	}
	if strings.Contains(string(bareData), "\"prompt\"") {
		t.Fatalf("empty prompt leaked into JSON: %s", bareData)
	}
}

func TestIssueJSONRejectsLeafWithoutStatus(t *testing.T) {
	payload := `{"id":"task-1","title":"Leaf","issue_type":"task","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","progress":{"total":1}}`
	var issue Issue
	err := json.Unmarshal([]byte(payload), &issue)
	if err == nil || !strings.Contains(err.Error(), "missing status field on non-epic") {
		t.Fatalf("Unmarshal() error = %v, want missing status field error", err)
	}
}

func TestNilLifecycleIssueLifecycleMethodsPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("State() on nil-lifecycle Issue did not panic")
		}
	}()
	_ = Issue{ID: "task-1", IssueType: "task"}.State()
}

func TestNeedsStoreHydrationLifecycleMethodsReturnZero(t *testing.T) {
	var issue Issue
	issue.ID = "epic-1"
	issue.IssueType = "epic"
	issue.pendingHydration = true
	if issue.State() != "" {
		t.Fatalf("State() = %q, want zero", issue.State())
	}
	if issue.Progress() != (Progress{}) {
		t.Fatalf("Progress() = %#v, want zero", issue.Progress())
	}
	if issue.Capabilities() != (Capabilities{}) {
		t.Fatalf("Capabilities() = %#v, want empty", issue.Capabilities())
	}
	if actions := issue.AvailableActions(); actions != nil {
		t.Fatalf("AvailableActions() = %#v, want nil", actions)
	}
}

func TestNilLifecycleIssueMarshalJSONErrors(t *testing.T) {
	_, err := json.Marshal(Issue{ID: "task-1", IssueType: "task"})
	if err == nil || !strings.Contains(err.Error(), "has no hydrated lifecycle") {
		t.Fatalf("Marshal() error = %v, want no hydrated lifecycle error", err)
	}
}

func TestIssueJSONOmitsProgress(t *testing.T) {
	issue := hydratedIssue(t, Issue{ID: "task-1", IssueType: "task"}, StateOpen)
	data, err := json.Marshal(issue)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, ok := payload["progress"]; ok {
		t.Fatalf("Marshal() included progress field: %s", data)
	}
}

func TestIsContainerUsesIssueTypeNotLifecycle(t *testing.T) {
	leaf := Issue{ID: "task-1", IssueType: "task"}
	if leaf.IsContainer() {
		t.Fatalf("unhydrated leaf reports IsContainer() = true; want false")
	}
	epic := Issue{ID: "epic-1", IssueType: "epic"}
	if !epic.IsContainer() {
		t.Fatalf("unhydrated epic reports IsContainer() = false; want true")
	}
}

func TestContainerTypesIsSubsetOfValidTypes(t *testing.T) {
	valid := map[string]bool{}
	for _, value := range ValidIssueTypes {
		valid[value] = true
	}
	for _, container := range ContainerIssueTypes {
		if !valid[container] {
			t.Fatalf("ContainerIssueTypes contains %q which is not in ValidIssueTypes", container)
		}
	}
}
