package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model/lifecycle"
)

type State = lifecycle.State
type Progress = lifecycle.Progress
type ActionName = lifecycle.ActionName

const (
	StateOpen       = lifecycle.Open
	StateInProgress = lifecycle.InProgress
	StateClosed     = lifecycle.Closed

	ActionStart  = lifecycle.ActionStart
	ActionDone   = lifecycle.ActionDone
	ActionClose  = lifecycle.ActionClose
	ActionReopen = lifecycle.ActionReopen
)

var (
	ParseState  = lifecycle.ParseState
	ParseAction = lifecycle.ParseAction
	DefaultOpen = lifecycle.DefaultOpen
)

func IsContainerType(issueType string) bool {
	trimmed := strings.TrimSpace(issueType)
	for _, container := range ContainerIssueTypes {
		if container == trimmed {
			return true
		}
	}
	return false
}

// ValidIssueTypes is the canonical set of issue types. ContainerIssueTypes is
// the subset that uses container-style lifecycle (no OwnedStatus).
// [LAW:one-source-of-truth] Issue-type vocabulary lives here; persistence validation and lifecycle dispatch both consult these sets.
var (
	ValidIssueTypes     = []string{"task", "feature", "bug", "chore", "epic"}
	ContainerIssueTypes = []string{"epic"}
)

func IsValidIssueType(issueType string) bool {
	trimmed := strings.TrimSpace(issueType)
	for _, valid := range ValidIssueTypes {
		if valid == trimmed {
			return true
		}
	}
	return false
}

// [LAW:one-type-per-behavior] Issues and epics are one record type; lifecycle
// capability data carries the behavior distinction without splitting shared
// issue behavior across duplicate types.
type Issue struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Prompt      string     `json:"prompt,omitempty"`
	Priority    int        `json:"priority"`
	IssueType   string     `json:"issue_type"`
	Topic       string     `json:"topic"`
	Rank        string     `json:"rank"`
	Labels      []string   `json:"labels"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`

	lifecycle        lifecycle.Lifecycle
	pendingHydration bool
}

func (i Issue) State() State {
	lifecycle, err := i.lifecycleOrError()
	if err != nil {
		return ""
	}
	return State(lifecycle.State())
}

func (i Issue) Progress() Progress {
	lifecycle, err := i.lifecycleOrError()
	if err != nil {
		return Progress{}
	}
	return lifecycle.Progress()
}

func (i Issue) Capabilities() Capabilities {
	lifecycle, err := i.lifecycleOrError()
	if err != nil {
		return Capabilities{}
	}
	return capabilitiesFrom(lifecycle)
}

func (i Issue) AvailableActions() []ActionName {
	root, err := i.lifecycleOrError()
	if err != nil {
		return nil
	}
	actionable, ok := root.(lifecycle.Actionable)
	if !ok {
		return nil
	}
	return actionable.AvailableActions()
}

// Apply is root-only: it only invokes actions exposed by the root lifecycle
// primitive. Multi-OwnedStatus composition (AllOf containing multiple
// actionable members) is intentionally unsupported here; that requires a
// dedicated disambiguation design before AllOf.Apply ever returns non-nil.
func (i Issue) Apply(action ActionName, actor string, reason string) (Issue, error) {
	root, err := i.lifecycleOrError()
	if err != nil {
		return Issue{}, err
	}
	actionable, ok := root.(lifecycle.Actionable)
	if !ok {
		return Issue{}, fmt.Errorf("no %s action available on this issue", action)
	}
	available := false
	for _, candidate := range actionable.AvailableActions() {
		if candidate == lifecycle.ActionName(action) {
			available = true
			break
		}
	}
	if !available {
		return Issue{}, fmt.Errorf("no %s action available on this issue", action)
	}
	next, err := actionable.Apply(lifecycle.ActionName(action), actor, reason)
	if err != nil {
		return Issue{}, err
	}
	i.replaceLifecycle(next)
	return i, nil
}

func (i Issue) StatusValue() string {
	status := i.Capabilities().Status
	if status == nil {
		return ""
	}
	return string(status.Value)
}

func (i Issue) AssigneeValue() string {
	status := i.Capabilities().Status
	if status == nil {
		return ""
	}
	return status.Assignee
}

func (i Issue) ClosedAtValue() *time.Time {
	status := i.Capabilities().Status
	if status == nil {
		return nil
	}
	return cloneTime(status.ClosedAt)
}

func (i Issue) IsContainer() bool {
	return IsContainerType(i.IssueType)
}

// IsHydrated reports whether this issue carries a fully-hydrated lifecycle.
// Returns false for issues constructed without HydrateOwnedStatus/HydrateAllOf
// and for JSON-decoded containers that have not yet passed through store
// hydration.
func (i Issue) IsHydrated() bool {
	if i.pendingHydration {
		return false
	}
	return i.lifecycle != nil
}

// HydrateOwnedStatus is the model-owned boundary that turns persisted row
// status fields into the lifecycle expression stored inside Issue.
// [LAW:single-enforcer] Row status fields become lifecycle state only through this model API.
func HydrateOwnedStatus(issue Issue, view StatusView) (Issue, error) {
	state := lifecycle.DefaultOpen(string(view.Value))
	issue.replaceLifecycle(lifecycle.OwnedStatus{
		Value:    state,
		Assignee: view.Assignee,
		ClosedAt: cloneTime(view.ClosedAt),
	})
	return issue, nil
}

func (i *Issue) replaceLifecycle(next lifecycle.Lifecycle) {
	// [LAW:single-enforcer] Lifecycle replacement is centralized inside model so callers cannot grow parallel mutation paths.
	i.lifecycle = next
	i.pendingHydration = false
}

// HydrateRow is the single shape-dispatch entry point: it picks AllOf vs
// OwnedStatus based on issue type and applies the matching hydrator. Callers
// that have already loaded both the row's status view and (for containers) the
// child issues should route through this function instead of repeating the
// IsContainerType discriminator.
// [LAW:single-enforcer] Container-vs-leaf hydration dispatch lives here so
// read paths don't grow parallel branches that drift apart.
func HydrateRow(issue Issue, view StatusView, children []Issue) (Issue, error) {
	if IsContainerType(issue.IssueType) {
		return HydrateAllOf(issue, children)
	}
	return HydrateOwnedStatus(issue, view)
}

// HydrateAllOf composes child issue lifecycles into a non-actionable container.
// [LAW:one-source-of-truth] Container state is derived from child lifecycles, never copied into another persisted field.
func HydrateAllOf(issue Issue, children []Issue) (Issue, error) {
	members := make([]lifecycle.Lifecycle, 0, len(children))
	for _, child := range children {
		lifecycle, err := child.lifecycleOrError()
		if err != nil {
			return Issue{}, err
		}
		members = append(members, lifecycle)
	}
	issue.replaceLifecycle(lifecycle.AllOf{Members: members})
	return issue, nil
}

// UpdateStatusCapability replaces the root status primitive and refuses
// containers so callers cannot silently corrupt derived lifecycle state.
func UpdateStatusCapability(issue Issue, view StatusView) (Issue, error) {
	root, err := issue.lifecycleOrError()
	if err != nil {
		return Issue{}, err
	}
	if _, ok := root.(lifecycle.OwnedStatus); !ok {
		return Issue{}, fmt.Errorf("issue %s does not expose a status capability", issue.ID)
	}
	return HydrateOwnedStatus(issue, view)
}

func (i Issue) lifecycleOrError() (lifecycle.Lifecycle, error) {
	if i.pendingHydration {
		return nil, fmt.Errorf("issue %s requires store hydration", i.ID)
	}
	if i.lifecycle == nil {
		panic(fmt.Sprintf("issue %q has no lifecycle (constructed without HydrateOwnedStatus/HydrateAllOf)", i.ID))
	}
	return i.lifecycle, nil
}

type issueJSON struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Prompt      string     `json:"prompt,omitempty"`
	Status      *State     `json:"status,omitempty"`
	Priority    int        `json:"priority"`
	IssueType   string     `json:"issue_type"`
	Topic       string     `json:"topic"`
	Assignee    string     `json:"assignee,omitempty"`
	Rank        string     `json:"rank"`
	Labels      []string   `json:"labels"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

func (i Issue) MarshalJSON() ([]byte, error) {
	if i.pendingHydration {
		return nil, fmt.Errorf("issue %s requires store hydration", i.ID)
	}
	if i.lifecycle == nil {
		// [LAW:single-enforcer] JSON serialization is the boundary that turns
		// unhydrated issue values into errors instead of process panics.
		return nil, fmt.Errorf("issue %s has no hydrated lifecycle", i.ID)
	}
	caps := capabilitiesFrom(i.lifecycle)
	if _, err := i.lifecycleOrError(); err != nil {
		return nil, err
	}
	var statusValue *State
	var assignee string
	var closedAt *time.Time
	if caps.Status != nil {
		value := caps.Status.Value
		statusValue = &value
		assignee = caps.Status.Assignee
		closedAt = cloneTime(caps.Status.ClosedAt)
	}
	return json.Marshal(issueJSON{
		ID:          i.ID,
		Title:       i.Title,
		Description: i.Description,
		Prompt:      i.Prompt,
		Status:      statusValue,
		Priority:    i.Priority,
		IssueType:   i.IssueType,
		Topic:       i.Topic,
		Assignee:    assignee,
		Rank:        i.Rank,
		Labels:      i.Labels,
		CreatedAt:   i.CreatedAt,
		UpdatedAt:   i.UpdatedAt,
		ClosedAt:    closedAt,
		ArchivedAt:  i.ArchivedAt,
		DeletedAt:   i.DeletedAt,
	})
}

func (i *Issue) UnmarshalJSON(data []byte) error {
	var payload issueJSON
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	*i = Issue{
		ID:          payload.ID,
		Title:       payload.Title,
		Description: payload.Description,
		Prompt:      payload.Prompt,
		Priority:    payload.Priority,
		IssueType:   payload.IssueType,
		Topic:       payload.Topic,
		Rank:        payload.Rank,
		Labels:      payload.Labels,
		CreatedAt:   payload.CreatedAt,
		UpdatedAt:   payload.UpdatedAt,
		ArchivedAt:  payload.ArchivedAt,
		DeletedAt:   payload.DeletedAt,
	}
	switch {
	case IsContainerType(payload.IssueType):
		// [LAW:single-enforcer] JSON cannot synthesize derived container lifecycle; store hydration is the only boundary that may attach child state.
		i.pendingHydration = true
		i.lifecycle = nil
	case payload.Status != nil:
		hydrated, err := HydrateOwnedStatus(*i, StatusView{
			Value:    *payload.Status,
			Assignee: payload.Assignee,
			ClosedAt: cloneTime(payload.ClosedAt),
		})
		if err != nil {
			return err
		}
		*i = hydrated
	default:
		return fmt.Errorf("issue %s: cannot hydrate lifecycle from JSON (missing status field on non-epic)", payload.ID)
	}
	return nil
}

type Relation struct {
	SrcID     string    `json:"src_id"`
	DstID     string    `json:"dst_id"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

type Comment struct {
	ID        string    `json:"id"`
	IssueID   string    `json:"issue_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

type Label struct {
	IssueID   string    `json:"issue_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

type IssueHistory struct {
	ID         string    `json:"id"`
	IssueID    string    `json:"issue_id"`
	Action     string    `json:"action"`
	Reason     string    `json:"reason"`
	FromStatus string    `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	CreatedAt  time.Time `json:"created_at"`
	CreatedBy  string    `json:"created_by"`
}

type IssueDetail struct {
	Issue     Issue          `json:"issue"`
	Relations []Relation     `json:"relations"`
	Comments  []Comment      `json:"comments"`
	Children  []Issue        `json:"children"`
	DependsOn []Issue        `json:"depends_on"`
	Related   []Issue        `json:"related"`
	Blocks    []Issue        `json:"blocks"`
	Parent    *Issue         `json:"parent,omitempty"`
	History   []IssueHistory `json:"history"`
}

type Export struct {
	Version     int            `json:"version"`
	WorkspaceID string         `json:"workspace_id"`
	ExportedAt  time.Time      `json:"exported_at"`
	Issues      []Issue        `json:"issues"`
	Relations   []Relation     `json:"relations"`
	Comments    []Comment      `json:"comments"`
	Labels      []Label        `json:"labels"`
	History     []IssueHistory `json:"history"`
}
