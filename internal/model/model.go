package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model/lifecycle"
)

type State = lifecycle.State
type Progress = lifecycle.Progress
type ActionName = lifecycle.ActionName
type Resolution = lifecycle.Resolution

type Retention = lifecycle.Retention
type Live = lifecycle.Live
type Archived = lifecycle.Archived
type Deleted = lifecycle.Deleted

type Action = lifecycle.Action
type StatusAction = lifecycle.StatusAction
type Start = lifecycle.Start
type Done = lifecycle.Done
type Close = lifecycle.Close
type Reopen = lifecycle.Reopen
type Archive = lifecycle.Archive
type Unarchive = lifecycle.Unarchive
type Delete = lifecycle.Delete
type Restore = lifecycle.Restore

type Outcome = lifecycle.Outcome
type Duplicate = lifecycle.Duplicate
type Superseded = lifecycle.Superseded
type Obsolete = lifecycle.Obsolete
type Wontfix = lifecycle.Wontfix

const (
	StateOpen       = lifecycle.Open
	StateInProgress = lifecycle.InProgress
	StateClosed     = lifecycle.Closed

	ActionStart  = lifecycle.ActionStart
	ActionDone   = lifecycle.ActionDone
	ActionClose  = lifecycle.ActionClose
	ActionReopen = lifecycle.ActionReopen

	ActionArchive   = lifecycle.ActionArchive
	ActionUnarchive = lifecycle.ActionUnarchive
	ActionDelete    = lifecycle.ActionDelete
	ActionRestore   = lifecycle.ActionRestore

	ResolutionDuplicate  = lifecycle.ResolutionDuplicate
	ResolutionSuperseded = lifecycle.ResolutionSuperseded
	ResolutionObsolete   = lifecycle.ResolutionObsolete
	ResolutionWontfix    = lifecycle.ResolutionWontfix
)

var (
	ParseState      = lifecycle.ParseState
	ParseAction     = lifecycle.ParseAction
	DefaultOpen     = lifecycle.DefaultOpen
	ParseResolution = lifecycle.ParseResolution

	RetentionFromTimestamps = lifecycle.RetentionFromTimestamps
	RetentionTimestamps     = lifecycle.RetentionTimestamps
	Retain                  = lifecycle.Retain
	Frozen                  = lifecycle.Frozen
)

// Priority constants for the two-level priority system.
// [LAW:one-source-of-truth] Canonical priority values live here; all other
// references derive from these constants rather than repeating magic ints.
const (
	PriorityNormal = 0
	PriorityUrgent = 1
)

// PriorityName returns the display name for a priority value.
func PriorityName(p int) string {
	switch p {
	case PriorityUrgent:
		return "urgent"
	default:
		return "normal"
	}
}

// [LAW:one-type-per-behavior] Issues and epics are one record type; lifecycle
// capability data carries the behavior distinction without splitting shared
// issue behavior across duplicate types.
type Issue struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Prompt      string `json:"prompt,omitempty"`
	Priority    int       `json:"priority"`
	IssueType   IssueType `json:"issue_type"`
	Topic       string    `json:"topic"`
	// Assignee is the issue's owner — orthogonal to the status state machine and
	// preserved across every transition. [LAW:one-source-of-truth] One home for
	// ownership; the lifecycle leaf carries no assignee.
	Assignee string `json:"assignee,omitempty"`
	Rank     string `json:"rank"`
	// Lane partitions an epic's children into parallel rank-ordered
	// sub-sequences: same lane → sequenced by rank, different lanes → parallel.
	// Empty string is the shared default lane (fully-sequential). Meaningful
	// only within an epic; the readiness gate is what scopes it.
	Lane      string    `json:"lane"`
	Labels    []string  `json:"labels"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// retention is the sealed retention axis (Live | Archived | Deleted). The
	// wire and storage encodings keep the legacy archived_at/deleted_at pair,
	// projected through lifecycle.RetentionTimestamps/RetentionFromTimestamps at
	// the serialization boundaries. [LAW:types-are-the-program] One value where
	// two nullable timestamps once left the archived+deleted combo representable.
	retention lifecycle.Retention

	lifecycle        lifecycle.Lifecycle
	pendingHydration bool
}

// Retention reports the issue's retention state. The zero value of the axis is
// Live — an issue constructed without an explicit retention is in the flow —
// so the nil interface normalizes here rather than forcing every construction
// site to state the origin. SetRetention refuses everything but the sealed
// value variants, so the field is provably either true nil (never set) or a
// legal variant, and this normalization is complete. [LAW:types-are-the-program]
func (i Issue) Retention() lifecycle.Retention {
	if i.retention == nil {
		return lifecycle.Live{}
	}
	return i.retention
}

// SetRetention replaces the retention state.
// [LAW:single-enforcer] Mirrors replaceLifecycle: retention changes flow
// through this one mutator, not ad-hoc field writes.
// [LAW:no-silent-failure] Go admits impostors behind the sealed interface — a
// typed-nil pointer variant, or raw nil — which readers would silently
// misclassify (a live issue reading as dead, retention collapsing on write).
// The one mutator refuses them, so no reader needs a guard.
func (i *Issue) SetRetention(r lifecycle.Retention) {
	switch r.(type) {
	case lifecycle.Live, lifecycle.Archived, lifecycle.Deleted:
		i.retention = r
	default:
		panic(fmt.Sprintf("issue %q: illegal Retention value %T", i.ID, r))
	}
}

// State and Progress are derived from the issue's children for a container and
// from the leaf primitive otherwise — both genuinely require a hydrated
// lifecycle, so they fail loud on an unhydrated issue rather than returning a
// zero value. An empty State() aliases a legitimately-open issue and a zero
// Progress aliases an empty container; both then flow into merge ranking,
// readiness, and column formatting as plausible wrong data far from the missing
// hydration call. [LAW:no-silent-failure] The unhydrated condition is surfaced,
// matching lifecycleOrError's own nil-lifecycle panic and MarshalJSON's error.
func (i Issue) State() State {
	return State(i.mustLifecycle().State())
}

func (i Issue) Progress() Progress {
	return i.mustLifecycle().Progress()
}

// Capabilities reports the issue's structural lifecycle capabilities. Whether an
// issue exposes a status capability is fixed by its type — leaves do, containers
// (whose state is derived from children) never do — so a container answers
// empty without a hydrated lifecycle, and that empty is the true answer rather
// than a swallowed error: it cannot alias a leaf, which always carries a Status.
// A leaf, by contrast, must be hydrated to answer, so mustLifecycle fails loud.
// [LAW:types-are-the-program] Container-has-no-status is a structural fact of the
// issue type, not a value that requires reading a possibly-absent lifecycle;
// State/Progress — which ARE child-derived — still demand hydration above.
func (i Issue) Capabilities() Capabilities {
	if i.IsContainer() {
		return Capabilities{}
	}
	return capabilitiesFrom(i.mustLifecycle())
}

// mustLifecycle is the single enforcer for lifecycle reads that have no error
// channel to a caller. An unhydrated read is a programmer error — the store
// boundary hydrates every issue before lifecycle state is read — so it fails
// loud rather than returning a zero value the caller cannot distinguish from a
// real state. [LAW:no-silent-failure] The recoverable callers of lifecycleOrError
// keep its error return; only these no-error-channel accessors route through here.
func (i Issue) mustLifecycle() lifecycle.Lifecycle {
	root, err := i.lifecycleOrError()
	if err != nil {
		panic(fmt.Sprintf("issue %q: lifecycle read on unhydrated issue: %v", i.ID, err))
	}
	return root
}

// ContainerActionError rejects a lifecycle action on a container, whose state
// derives from its children and cannot be set directly. It carries the issue
// ID and the live child-state breakdown so the message can tell the agent what
// to do next — facts the lifecycle leaf has no access to, which is why this
// rejection is owned here at the dispatch boundary.
// [LAW:single-enforcer] Error() is the only source of the container-rejection
// wording; tests and callers discriminate on the type, not the prose.
type ContainerActionError struct {
	ID       string
	Action   ActionName
	Progress Progress
}

// Unfinished reports how many children are not yet done — the live count the
// rejection message carries.
func (e ContainerActionError) Unfinished() int {
	return e.Progress.Total - e.Progress.Closed
}

// [LAW:dataflow-not-control-flow] The wording varies with the progress values
// the error carries, not with which callsite produced it.
func (e ContainerActionError) Error() string {
	switch {
	case e.Progress.Total == 0:
		return fmt.Sprintf("epic %s has no children; an epic's state derives from its children and cannot be set directly", e.ID)
	case e.Unfinished() == 0:
		return fmt.Sprintf("epic %s is already closed: all %d children are done, and an epic's state derives from its children", e.ID, e.Progress.Total)
	default:
		return fmt.Sprintf("epic %s has %d children that are not done. Complete the children to close the epic", e.ID, e.Unfinished())
	}
}

// Apply is root-only: it dispatches to the root lifecycle primitive's Apply.
// Multi-leaf composition (AllOf containing multiple actionable members)
// is intentionally unsupported here; that requires a dedicated disambiguation
// design before containers ever become actionable. Containers reject every
// action because their state is derived from children — structurally: AllOf
// does not implement Actionable, and the Container branch here composes the
// epic-aware rejection from the issue ID and child progress that only this
// boundary holds.
// [LAW:types-are-the-program] No idempotent / from-state branching here: the
// leaf's Apply is target-state, so same-state inputs round-trip through the
// leaf and back unchanged; the only rejections that survive are the real
// invariants (unhydrated lifecycle, container here) — the leaf itself cannot
// fail, since StatusAction makes an unsupported action unrepresentable.
func (i Issue) Apply(action lifecycle.StatusAction) (Issue, error) {
	root, err := i.lifecycleOrError()
	if err != nil {
		return Issue{}, err
	}
	if _, ok := root.(lifecycle.Container); ok {
		return Issue{}, ContainerActionError{ID: i.ID, Action: action.Name(), Progress: root.Progress()}
	}
	actionable, ok := root.(lifecycle.Actionable)
	if !ok {
		return Issue{}, fmt.Errorf("no %s action available on this issue", action.Name())
	}
	i.replaceLifecycle(actionable.Apply(action))
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
	return i.Assignee
}

func (i Issue) ClosedAtValue() *time.Time {
	status := i.Capabilities().Status
	if status == nil {
		return nil
	}
	return cloneTime(status.ClosedAt)
}

// ResolutionValue is the close reason projected to the issue level, nil unless
// the issue is closed with a recorded resolution. Read through the capability
// seam so the accessor stays oblivious to which leaf variant backs the issue.
func (i Issue) ResolutionValue() *lifecycle.Resolution {
	status := i.Capabilities().Status
	if status == nil {
		return nil
	}
	return cloneResolution(status.Resolution)
}

// RedirectTargetValue is the canonical ticket a redirecting close points to,
// projected to the issue level; nil unless the issue is closed with a
// redirecting resolution that carries its target. Read through the capability
// seam like ResolutionValue — the target is that resolution's payload.
func (i Issue) RedirectTargetValue() *string {
	status := i.Capabilities().Status
	if status == nil {
		return nil
	}
	return cloneString(status.RedirectTarget)
}

func (i Issue) IsContainer() bool {
	return i.IssueType.IsContainer()
}

// IsHydrated reports whether this issue carries a fully-hydrated lifecycle.
// Returns false for issues constructed without HydrateStatus/HydrateAllOf
// and for JSON-decoded containers that have not yet passed through store
// hydration.
func (i Issue) IsHydrated() bool {
	if i.pendingHydration {
		return false
	}
	return i.lifecycle != nil
}

// HydrateStatus is the model-owned boundary that turns a persisted row's status
// fields into the leaf lifecycle expression stored inside Issue. Assignee is not
// a lifecycle field, so it is carried on Issue.Assignee, not through here.
// [LAW:single-enforcer] Row status fields become lifecycle state only through this model API.
func HydrateStatus(issue Issue, view StatusView) (Issue, error) {
	issue.replaceLifecycle(lifecycle.NewStatus(view.Value, view.ClosedAt, view.Resolution, view.RedirectTarget))
	return issue, nil
}

func (i *Issue) replaceLifecycle(next lifecycle.Lifecycle) {
	// [LAW:single-enforcer] Lifecycle replacement is centralized inside model so callers cannot grow parallel mutation paths.
	i.lifecycle = next
	i.pendingHydration = false
}

// HydrateRow is the single shape-dispatch entry point: it picks AllOf vs the
// leaf status primitive based on issue type and applies the matching hydrator. Callers
// that have already loaded both the row's status view and (for containers) the
// child issues should route through this function instead of repeating the
// container discriminator.
// [LAW:single-enforcer] Container-vs-leaf hydration dispatch lives here so
// read paths don't grow parallel branches that drift apart.
func HydrateRow(issue Issue, view StatusView, children []Issue) (Issue, error) {
	if issue.IssueType.IsContainer() {
		return HydrateAllOf(issue, children)
	}
	return HydrateStatus(issue, view)
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

func (i Issue) lifecycleOrError() (lifecycle.Lifecycle, error) {
	if i.pendingHydration {
		return nil, fmt.Errorf("issue %s requires store hydration", i.ID)
	}
	if i.lifecycle == nil {
		panic(fmt.Sprintf("issue %q has no lifecycle (constructed without HydrateStatus/HydrateAllOf)", i.ID))
	}
	return i.lifecycle, nil
}

type issueJSON struct {
	ID          string                `json:"id"`
	Title       string                `json:"title"`
	Description string                `json:"description"`
	Prompt      string                `json:"prompt,omitempty"`
	Status      *State                `json:"status,omitempty"`
	Priority    int                   `json:"priority"`
	IssueType   IssueType             `json:"issue_type"`
	Topic       string                `json:"topic"`
	Assignee    string                `json:"assignee,omitempty"`
	Rank        string                `json:"rank"`
	Lane        string                `json:"lane"`
	Labels      []string              `json:"labels"`
	CreatedAt   time.Time             `json:"created_at"`
	UpdatedAt   time.Time             `json:"updated_at"`
	ClosedAt    *time.Time            `json:"closed_at,omitempty"`
	Resolution  *lifecycle.Resolution `json:"resolution,omitempty"`
	// RedirectTarget rides beside Resolution on the wire exactly as it does in
	// the closed leaf and the issues row: the redirecting resolution's payload.
	RedirectTarget *string    `json:"redirect_target,omitempty"`
	ArchivedAt     *time.Time `json:"archived_at,omitempty"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
}

// IssueWireFields lists the JSON keys of the serialized issue object, derived
// from the one wire struct MarshalJSON emits. Issue's struct fields are the
// in-memory shape, not the wire shape — status, closed_at, resolution, and the
// retention pair exist only on the wire — so consumers validating field names
// against the serialized form must read this set, never reflect over Issue.
// [LAW:one-source-of-truth] Derived from issueJSON itself, so the set cannot
// drift from what MarshalJSON writes.
func IssueWireFields() []string {
	wire := reflect.TypeOf(issueJSON{})
	names := make([]string, 0, wire.NumField())
	for i := 0; i < wire.NumField(); i++ {
		field := wire.Field(i)
		// encoding/json's tag contract: "-" excludes the field from the wire;
		// an absent tag or empty tag name marshals under the Go field name.
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		names = append(names, name)
	}
	return names
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
	var closedAt *time.Time
	var resolution *lifecycle.Resolution
	var redirectTarget *string
	if caps.Status != nil {
		value := caps.Status.Value
		statusValue = &value
		closedAt = cloneTime(caps.Status.ClosedAt)
		resolution = cloneResolution(caps.Status.Resolution)
		redirectTarget = cloneString(caps.Status.RedirectTarget)
	}
	archivedAt, deletedAt := lifecycle.RetentionTimestamps(i.Retention())
	return json.Marshal(issueJSON{
		ID:             i.ID,
		Title:          i.Title,
		Description:    i.Description,
		Prompt:         i.Prompt,
		Status:         statusValue,
		Priority:       i.Priority,
		IssueType:      i.IssueType,
		Topic:          i.Topic,
		Assignee:       i.Assignee,
		Rank:           i.Rank,
		Lane:           i.Lane,
		Labels:         i.Labels,
		CreatedAt:      i.CreatedAt,
		UpdatedAt:      i.UpdatedAt,
		ClosedAt:       closedAt,
		Resolution:     resolution,
		RedirectTarget: redirectTarget,
		ArchivedAt:     archivedAt,
		DeletedAt:      deletedAt,
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
		Assignee:    payload.Assignee,
		Rank:        payload.Rank,
		Lane:        payload.Lane,
		Labels:      payload.Labels,
		CreatedAt:   payload.CreatedAt,
		UpdatedAt:   payload.UpdatedAt,
		retention:   lifecycle.RetentionFromTimestamps(payload.ArchivedAt, payload.DeletedAt),
	}
	switch {
	case payload.IssueType.IsContainer():
		// [LAW:single-enforcer] JSON cannot synthesize derived container lifecycle; store hydration is the only boundary that may attach child state.
		i.pendingHydration = true
		i.lifecycle = nil
	case payload.Status != nil:
		hydrated, err := HydrateStatus(*i, StatusView{
			Value:          *payload.Status,
			ClosedAt:       cloneTime(payload.ClosedAt),
			Resolution:     cloneResolution(payload.Resolution),
			RedirectTarget: cloneString(payload.RedirectTarget),
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
	SrcID     string       `json:"src_id"`
	DstID     string       `json:"dst_id"`
	Type      RelationType `json:"type"`
	CreatedAt time.Time    `json:"created_at"`
	CreatedBy string       `json:"created_by"`
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

// FieldChange describes a single field's transition within an IssueEvent.
// Both From and To are stringified (TEXT in the database) so the schema is
// field-agnostic — every issue field, regardless of its native type, lands
// in the same shape.
type FieldChange struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// IssueEvent is the field-agnostic history record. Every mutation to an
// issue — status transitions, archive/delete flips, plain field updates —
// produces one event with the actor + reason and N field-change rows for
// the fields that actually moved. Action is optional intent metadata
// populated by named status transitions (start/done/close/reopen/etc.) and
// left empty for plain field updates; per-field actions do not exist.
type IssueEvent struct {
	ID        string        `json:"id"`
	IssueID   string        `json:"issue_id"`
	Action    string        `json:"action,omitempty"`
	Reason    string        `json:"reason"`
	Actor     string        `json:"actor"`
	CreatedAt time.Time     `json:"created_at"`
	Changes   []FieldChange `json:"changes"`
}

type IssueDetail struct {
	Issue     Issue      `json:"issue"`
	Relations []Relation `json:"relations"`
	Comments  []Comment  `json:"comments"`
	Children  []Issue    `json:"children"`
	Siblings  []Issue    `json:"siblings"`
	DependsOn []Issue    `json:"depends_on"`
	Related   []Issue    `json:"related"`
	Blocks    []Issue    `json:"blocks"`
	Parent    *Issue     `json:"parent,omitempty"`
	// RedirectTarget is the canonical ticket a duplicate/superseded close
	// redirects to — the load-bearing "where did this work go" relationship,
	// hydrated from the issue's own redirect target, never from the relations
	// graph. Related carries only manual peer links, so the two render
	// independently: a redirect and a manual edge to the same ticket are two
	// facts, shown as two groups. [LAW:one-source-of-truth]
	RedirectTarget *Issue       `json:"redirect_target,omitempty"`
	Events         []IssueEvent `json:"events"`
}

type Export struct {
	Version     int          `json:"version"`
	WorkspaceID string       `json:"workspace_id"`
	ExportedAt  time.Time    `json:"exported_at"`
	Issues      []Issue      `json:"issues"`
	Relations   []Relation   `json:"relations"`
	Comments    []Comment    `json:"comments"`
	Labels      []Label      `json:"labels"`
	Events      []IssueEvent `json:"events"`
}

// v1ExportHistory is the legacy history row produced by Version 1 exports.
// Version 2 replaces the "history" array with the richer "events" schema.
type v1ExportHistory struct {
	IssueID    string    `json:"issue_id"`
	Action     string    `json:"action"`
	FromStatus string    `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	Reason     string    `json:"reason"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
}

// v1EventID derives a deterministic, collision-resistant ID from a v1 history
// row's content so that merging two v1 exports never produces duplicate IDs
// for different events. Identical rows produce the same ID (safe dedup); rows
// with any differing field produce a distinct ID (safe merge).
func v1EventID(issueID, action, fromStatus, toStatus, createdBy string, createdAt time.Time) string {
	key := strings.Join([]string{issueID, action, fromStatus, toStatus, createdBy, createdAt.Format(time.RFC3339Nano)}, "|")
	sum := sha256.Sum256([]byte(key))
	return "evt-v1-" + hex.EncodeToString(sum[:8])
}

// UnmarshalJSON handles both v1 (history array) and v2 (events array) export
// formats so old sync files and backup restores remain readable after the
// schema upgrade to Version 2.
// [LAW:single-enforcer] Version dispatch lives here; every JSON decode path
// (syncfile, backup, store tests) inherits it through json.Unmarshal.
func (e *Export) UnmarshalJSON(data []byte) error {
	type rawExport struct {
		Version     int               `json:"version"`
		WorkspaceID string            `json:"workspace_id"`
		ExportedAt  time.Time         `json:"exported_at"`
		Issues      []Issue           `json:"issues"`
		Relations   []Relation        `json:"relations"`
		Comments    []Comment         `json:"comments"`
		Labels      []Label           `json:"labels"`
		Events      []IssueEvent      `json:"events"`
		History     []v1ExportHistory `json:"history"`
	}
	var raw rawExport
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*e = Export{
		Version:     raw.Version,
		WorkspaceID: raw.WorkspaceID,
		ExportedAt:  raw.ExportedAt,
		Issues:      raw.Issues,
		Relations:   raw.Relations,
		Comments:    raw.Comments,
		Labels:      raw.Labels,
		Events:      raw.Events,
	}
	// v1 exports carry "history" rows instead of "events". Convert each row to
	// an IssueEvent with a single status field-change so ReplaceFromExport and
	// merging work without special-casing the version downstream.
	if raw.Version < 2 && len(raw.History) > 0 {
		for _, h := range raw.History {
			e.Events = append(e.Events, IssueEvent{
				ID:        v1EventID(h.IssueID, h.Action, h.FromStatus, h.ToStatus, h.CreatedBy, h.CreatedAt),
				IssueID:   h.IssueID,
				Action:    h.Action,
				Reason:    h.Reason,
				Actor:     h.CreatedBy,
				CreatedAt: h.CreatedAt,
				Changes:   []FieldChange{{Field: "status", From: h.FromStatus, To: h.ToStatus}},
			})
		}
	}
	return nil
}
