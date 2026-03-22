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
	missingFieldDef     = &kindDef{key: "missing_field"}
	blockedByDef        = &kindDef{key: "blocked_by"}
	priorityInversionDef = &kindDef{key: "priority_inversion"}

	MissingField      = Kind{def: missingFieldDef}      // a required field is empty or unset
	BlockedBy         = Kind{def: blockedByDef}          // issue depends on an open ticket
	PriorityInversion = Kind{def: priorityInversionDef}  // blocker has worse priority than dependent

	// [LAW:single-enforcer] The registry is the single authority for valid kinds.
	kindRegistry = map[string]Kind{
		missingFieldDef.key:      MissingField,
		blockedByDef.key:         BlockedBy,
		priorityInversionDef.key: PriorityInversion,
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

// AnnotatedIssue pairs an issue with its computed annotations.
// [LAW:one-type-per-behavior] All issues flow through this single type regardless
// of what annotations they carry. Consumers interpret annotations via predicates.
type AnnotatedIssue struct {
	model.Issue
	Annotations []Annotation `json:"annotations"`
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
