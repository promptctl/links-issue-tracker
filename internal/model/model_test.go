package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func hydratedIssue(t *testing.T, issue Issue, status State) Issue {
	t.Helper()
	hydrated, err := HydrateStatus(issue, StatusView{Value: status})
	if err != nil {
		t.Fatalf("HydrateStatus() error = %v", err)
	}
	return hydrated
}

func TestApplyRefusesContainerForEveryAction(t *testing.T) {
	childA := hydratedIssue(t, Issue{ID: "a", IssueType: "task"}, StateOpen)
	childB := hydratedIssue(t, Issue{ID: "b", IssueType: "task"}, StateOpen)
	container, err := HydrateAllOf(Issue{ID: "epic", IssueType: "epic"}, []Issue{childA, childB})
	if err != nil {
		t.Fatalf("HydrateAllOf() error = %v", err)
	}
	for _, action := range []StatusAction{Start{}, Done{}, Close{Outcome: Wontfix{}}, Reopen{}} {
		if _, err := container.Apply(action); err == nil {
			t.Fatalf("Apply(%s on epic) error = nil, want container rejection", action.Name())
		}
	}
}

// TestApplyTargetStateOnLeafProducesTargetState exercises every (from, action)
// pair on a hydrated leaf to confirm Issue.Apply now obeys the target-state
// contract: action determines the post-state regardless of from-state, and
// same-state pairs succeed as no-ops.
func TestApplyTargetStateOnLeafProducesTargetState(t *testing.T) {
	matrix := []StatusAction{Start{}, Done{}, Close{Outcome: Wontfix{}}, Reopen{}}
	for _, from := range []State{StateOpen, StateInProgress, StateClosed} {
		for _, action := range matrix {
			t.Run(string(from)+"_"+string(action.Name()), func(t *testing.T) {
				leaf := hydratedIssue(t, Issue{ID: "leaf", IssueType: "task"}, from)
				next, err := leaf.Apply(action)
				if err != nil {
					t.Fatalf("Apply(%s on %s) error = %v, want success", action.Name(), from, err)
				}
				if next.State() != action.Target() {
					t.Fatalf("Apply(%s on %s).State() = %q, want %q", action.Name(), from, next.State(), action.Target())
				}
			})
		}
	}
}

// TestApplyCloseOutcomeSurfacesThroughResolutionValue pins the issue-level
// accessor chain for the payload threading: a Close's outcome, having traveled
// through the state machine into the closed leaf, must surface through
// Issue.ResolutionValue(); the neutral Done records none.
func TestApplyCloseOutcomeSurfacesThroughResolutionValue(t *testing.T) {
	leaf := hydratedIssue(t, Issue{ID: "leaf", IssueType: "task"}, StateOpen)
	closed, err := leaf.Apply(Close{Outcome: Duplicate{Of: "links-abc1"}})
	if err != nil {
		t.Fatalf("Apply(close duplicate) error = %v", err)
	}
	got := closed.ResolutionValue()
	if got == nil || *got != ResolutionDuplicate {
		t.Fatalf("ResolutionValue() after duplicate close = %v, want %q", got, ResolutionDuplicate)
	}

	done, err := hydratedIssue(t, Issue{ID: "leaf2", IssueType: "task"}, StateInProgress).Apply(Done{})
	if err != nil {
		t.Fatalf("Apply(done) error = %v", err)
	}
	if got := done.ResolutionValue(); got != nil {
		t.Fatalf("ResolutionValue() after done = %v, want nil — done records no resolution", got)
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
	if _, err := json.Marshal(decoded); err == nil || !strings.Contains(err.Error(), "requires store hydration") {
		t.Fatalf("Marshal(decoded epic) error = %v, want hydration error", err)
	}
}

func TestIssueJSONRoundTripLeafPreservesStatusFields(t *testing.T) {
	closedAt := time.Now().UTC()
	leaf, err := HydrateStatus(Issue{
		ID:        "task-1",
		Title:     "Leaf",
		IssueType: "task",
		Assignee:  "dev",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, StatusView{Value: StateClosed, ClosedAt: &closedAt})
	if err != nil {
		t.Fatalf("HydrateStatus() error = %v", err)
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
	leaf, err := HydrateStatus(Issue{
		ID:        "task-1",
		Title:     "Leaf with prompt",
		IssueType: "task",
		Prompt:    "Run the renderer headless and assert no NaNs.",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, StatusView{Value: StateOpen})
	if err != nil {
		t.Fatalf("HydrateStatus() error = %v", err)
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
	bare, err := HydrateStatus(Issue{
		ID:        "task-2",
		Title:     "Leaf without prompt",
		IssueType: "task",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}, StatusView{Value: StateOpen})
	if err != nil {
		t.Fatalf("HydrateStatus(bare) error = %v", err)
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

func TestNeedsStoreHydrationChildDerivedReadsPanic(t *testing.T) {
	// State and Progress are derived from a container's children, so they cannot
	// be answered on a pendingHydration issue (a JSON-decoded container awaiting
	// store hydration). They must fail loud, not return a zero value that aliases
	// a legitimately open / empty issue downstream in merge, readiness, and column
	// formatting.
	newPending := func() Issue {
		var issue Issue
		issue.ID = "epic-1"
		issue.IssueType = "epic"
		issue.pendingHydration = true
		return issue
	}
	for _, tc := range []struct {
		name string
		read func(Issue)
	}{
		{"State", func(i Issue) { _ = i.State() }},
		{"Progress", func(i Issue) { _ = i.Progress() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("%s() on pendingHydration issue did not panic", tc.name)
				}
			}()
			tc.read(newPending())
		})
	}
}

func TestContainerCapabilitiesAreEmptyWithoutHydration(t *testing.T) {
	// A container exposes no status capability regardless of hydration — its state
	// is derived from children. So Capabilities() on a pendingHydration container
	// answers empty by type, the true answer, not a swallowed error: empty cannot
	// alias a leaf, which always carries a non-nil Status. This is what lets the
	// merge change-gate and the import path read a JSON-decoded container without
	// either a spurious panic or a wrong value.
	var issue Issue
	issue.ID = "epic-1"
	issue.IssueType = "epic"
	issue.pendingHydration = true
	if caps := issue.Capabilities(); caps != (Capabilities{}) {
		t.Fatalf("Capabilities() = %#v on unhydrated container, want empty", caps)
	}
}

func TestNilLifecycleLeafCapabilitiesPanic(t *testing.T) {
	// A leaf (non-container) must be hydrated to answer Capabilities(): its status
	// capability lives in the lifecycle, so an unhydrated leaf has no answer and
	// must fail loud rather than return an empty Capabilities indistinguishable
	// from the legitimately-empty container case.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Capabilities() on nil-lifecycle leaf did not panic")
		}
	}()
	_ = Issue{ID: "task-1", IssueType: "task"}.Capabilities()
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

// The one mutator is the gate that keeps impostors — values satisfying the
// sealed interface without being one of the three value variants — out of the
// retention field, so readers can trust every stored value without guards.
func TestSetRetentionRefusesImpostors(t *testing.T) {
	for name, impostor := range map[string]Retention{
		"typed-nil pointer variant": (*Archived)(nil),
		"raw nil":                   nil,
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("SetRetention(%s) did not panic", name)
				}
			}()
			issue := Issue{ID: "i1"}
			issue.SetRetention(impostor)
		}()
	}
}

// Every key MarshalJSON emits must be a valid wire-field name, so required-field
// validation (which consumes IssueWireFields) can never reject a field that the
// serialized form carries.
func TestIssueWireFieldsCoverMarshalOutput(t *testing.T) {
	now := time.Now().UTC()
	issue := Issue{ID: "i1", IssueType: "task", Labels: []string{}}
	issue.SetRetention(Archived{At: now})
	hydrated := hydratedIssue(t, issue, StateOpen)
	payload, err := json.Marshal(hydrated)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	valid := map[string]struct{}{}
	for _, name := range IssueWireFields() {
		valid[name] = struct{}{}
	}
	for key := range wire {
		if _, ok := valid[key]; !ok {
			t.Fatalf("MarshalJSON emits %q but IssueWireFields does not name it", key)
		}
	}
	for _, name := range []string{"archived_at", "deleted_at", "resolution", "status", "closed_at"} {
		if _, ok := valid[name]; !ok {
			t.Fatalf("IssueWireFields missing wire-only field %q", name)
		}
	}
}
