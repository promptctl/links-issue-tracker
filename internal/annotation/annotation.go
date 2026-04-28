package annotation

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bmf/links-issue-tracker/internal/model"
)

type kindDef struct {
	key string
}

// Kind identifies a category of annotation.
// The zero value is invalid; only the package registry produces valid kinds.
// [LAW:single-enforcer] New annotation types and kind validity are enforced here.
type Kind struct {
	def *kindDef
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

var (
	missingFieldDef   = &kindDef{key: "missing_field"}
	openDependencyDef = &kindDef{key: "open_dependency"}
	rankInversionDef  = &kindDef{key: "rank_inversion"}
	orphanedDef       = &kindDef{key: "orphaned"}

	MissingField   = Kind{def: missingFieldDef}   // a required field is empty or unset
	OpenDependency = Kind{def: openDependencyDef} // issue depends on an open ticket
	RankInversion  = Kind{def: rankInversionDef}  // dependency is ranked below the dependent
	Orphaned       = Kind{def: orphanedDef}       // in_progress with no update past the orphaned threshold

	// [LAW:single-enforcer] The registry is the single authority for valid kinds.
	// "blocked_by" is a deserialization alias for backwards compatibility after
	// the rename to "open_dependency".
	kindRegistry = map[string]Kind{
		missingFieldDef.key:   MissingField,
		openDependencyDef.key: OpenDependency,
		"blocked_by":          OpenDependency,
		rankInversionDef.key:  RankInversion,
		orphanedDef.key:       Orphaned,
	}
)

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
