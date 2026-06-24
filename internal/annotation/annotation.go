package annotation

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

type kindDef struct {
	key  string
	role ReadinessRole
}

// ReadinessRole is the readiness family a kind belongs to. Every kind declares
// one at construction, so whether a kind blocks readiness is a property carried
// by the kind itself — not a side-list a consumer maintains in parallel.
// [LAW:types-are-the-program] The disposition a switch would otherwise infer
// from a maintained slice is encoded where the kind is defined; a new kind
// cannot be added without classifying it (register requires this argument).
type ReadinessRole int

const (
	roleInvalid       ReadinessRole = iota // zero value: never a valid classification
	RoleBlocking                           // prevents pulling the issue now
	RoleOrphaned                           // staleness signal, not a blocker
	RoleRankInversion                      // rank-hygiene signal, not a blocker
	RoleNone                               // ordering/advisory only, invisible to readiness
)

// Kind identifies a category of annotation.
// The zero value is invalid; only the package registry produces valid kinds.
// [LAW:single-enforcer] New annotation types and kind validity are enforced here.
type Kind struct {
	def *kindDef
}

// ReadinessRole reports the readiness family this kind belongs to. The zero
// Kind has no role; ClassifyReadiness fails loudly rather than treating it as
// ready, so a corrupt or unclassified kind can never silently pass the gate.
func (k Kind) ReadinessRole() ReadinessRole {
	if k.def == nil {
		return roleInvalid
	}
	return k.def.role
}

// String returns the serialization key for this kind.
func (k Kind) String() string {
	if k.def == nil {
		return ""
	}
	return k.def.key
}

// MarshalJSON serializes the kind as a JSON string.
func (k Kind) MarshalJSON() ([]byte, error) {
	if k.def == nil {
		return nil, fmt.Errorf("marshal annotation kind: invalid kind")
	}
	return json.Marshal(k.def.key)
}

// UnmarshalJSON deserializes a JSON string into a Kind.
func (k *Kind) UnmarshalJSON(data []byte) error {
	var key string
	if err := json.Unmarshal(data, &key); err != nil {
		return err
	}
	parsed, ok := parseKind(key)
	if !ok {
		return fmt.Errorf("unknown annotation kind %q", key)
	}
	*k = parsed
	return nil
}

// [LAW:single-enforcer] The registry is the single authority for valid kinds,
// populated only by register — there is no second hand-maintained list to drift.
var (
	kindRegistry    = map[string]Kind{}
	registeredKinds []Kind // canonical kinds in declaration order, for stable enumeration
)

// register mints a kind, records it as the single authority for kind validity,
// and requires its readiness role. Requiring role at the one birth site is the
// compile-time gate: a new kind line cannot be written without classifying how
// it disposes toward readiness, and var, registry, and role are set together.
// [LAW:one-source-of-truth] One birth site for every kind.
func register(key string, role ReadinessRole) Kind {
	if role == roleInvalid {
		panic("annotation: kind " + key + " registered without a readiness role")
	}
	if _, exists := kindRegistry[key]; exists {
		panic("annotation: duplicate kind key " + key)
	}
	k := Kind{def: &kindDef{key: key, role: role}}
	kindRegistry[key] = k
	registeredKinds = append(registeredKinds, k)
	return k
}

var (
	MissingField          = register("missing_field", RoleBlocking)           // a required field is empty or unset
	OpenDependency        = register("open_dependency", RoleBlocking)         // issue depends on an open ticket
	RankInversion         = register("rank_inversion", RoleRankInversion)     // dependency is ranked below the dependent
	Orphaned              = register("orphaned", RoleOrphaned)                // in_progress with no update past the orphaned threshold
	NeedsDesign           = register("needs_design", RoleBlocking)            // carries the needs-design label
	EarlierSiblingPending = register("earlier_sibling_pending", RoleBlocking) // an earlier same-lane sibling under the parent epic is still open
	FocusPath             = register("focus_path", RoleNone)                  // a focused goal or a derived prerequisite of one; an ordering signal
)

func init() {
	// "blocked_by" is a deserialization alias for data written before the rename
	// to "open_dependency"; it resolves to the same kind, not a new one — so it
	// is registered as an alias here, never minted as its own kind.
	kindRegistry["blocked_by"] = OpenDependency
}

// Kinds returns all registered kinds in declaration order. Aliases are not
// included — only the canonical kinds register mints.
func Kinds() []Kind {
	return append([]Kind(nil), registeredKinds...)
}

func parseKind(key string) (Kind, bool) {
	kind, ok := kindRegistry[key]
	return kind, ok
}

// Annotation is a computed fact about an issue.
type Annotation struct {
	Kind    Kind   `json:"kind"`
	Message string `json:"message"`
}

// ParentEpicRef identifies an issue's containing epic by id and title.
// Present only when the issue has a parent AND the parent is type=epic —
// the single most important context for an agent deciding which leaf to
// claim (links-agent-epic-model-uew.2).
type ParentEpicRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// AnnotatedIssue pairs an issue with its computed annotations.
// [LAW:one-type-per-behavior] All issues flow through this single type regardless
// of what annotations they carry. Consumers interpret annotations via predicates.
type AnnotatedIssue struct {
	model.Issue
	Annotations []Annotation   `json:"annotations"`
	ParentEpic  *ParentEpicRef `json:"parent_epic,omitempty"`
}

func (a AnnotatedIssue) MarshalJSON() ([]byte, error) {
	var payload map[string]any
	issueData, err := json.Marshal(a.Issue)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(issueData, &payload); err != nil {
		return nil, err
	}
	payload["annotations"] = a.Annotations
	if a.ParentEpic != nil {
		payload["parent_epic"] = a.ParentEpic
	}
	return json.Marshal(payload)
}

func (a *AnnotatedIssue) UnmarshalJSON(data []byte) error {
	var issue model.Issue
	if err := json.Unmarshal(data, &issue); err != nil {
		return err
	}
	var payload struct {
		Annotations []Annotation   `json:"annotations"`
		ParentEpic  *ParentEpicRef `json:"parent_epic"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	a.Issue = issue
	a.Annotations = payload.Annotations
	a.ParentEpic = payload.ParentEpic
	return nil
}

// Annotator computes annotations for a single issue.
type Annotator func(ctx context.Context, issue model.Issue) ([]Annotation, error)

// Annotate applies all annotators to every issue unconditionally.
// [LAW:dataflow-not-control-flow] Every issue flows through every annotator.
// Variability is in the annotation values, not in whether annotators execute.
func Annotate(ctx context.Context, issues []model.Issue, annotators ...Annotator) ([]AnnotatedIssue, error) {
	result := make([]AnnotatedIssue, len(issues))
	for i, issue := range issues {
		var all []Annotation
		for _, annotator := range annotators {
			annotations, err := annotator(ctx, issue)
			if err != nil {
				return nil, err
			}
			all = append(all, annotations...)
		}
		if all == nil {
			all = []Annotation{}
		}
		result[i] = AnnotatedIssue{
			Issue:       issue,
			Annotations: all,
		}
	}
	return result, nil
}

// HasAny returns true if any annotation has a kind matching one of the given kinds.
// This is a neutral utility — the caller decides which kinds matter and why.
func HasAny(annotations []Annotation, kinds ...Kind) bool {
	for _, a := range annotations {
		for _, k := range kinds {
			if a.Kind == k {
				return true
			}
		}
	}
	return false
}
