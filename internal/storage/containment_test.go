package storage

import (
	"errors"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// The detach rule is judged on what the detach leaves: a leaf with a parent
// edge still held is inside an epic, and only a leaf left with none is refused.
// An edge of another type names no parent, so its child is never read.
func TestRejectDetachingParentEdge(t *testing.T) {
	cases := []struct {
		name        string
		relType     model.RelationType
		childType   model.IssueType
		parentsLeft int
		refused     bool
	}{
		{name: "a leaf left with no parent", relType: model.RelParentChild, childType: model.TypeTask, parentsLeft: 0, refused: true},
		{name: "a leaf that keeps a second parent", relType: model.RelParentChild, childType: model.TypeTask, parentsLeft: 1, refused: false},
		{name: "an epic left with no parent", relType: model.RelParentChild, childType: model.TypeEpic, parentsLeft: 0, refused: false},
		{name: "an epic that keeps a second parent", relType: model.RelParentChild, childType: model.TypeEpic, parentsLeft: 1, refused: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RejectDetachingParentEdge(tc.relType, "c", func() (model.IssueType, int, error) {
				return tc.childType, tc.parentsLeft, nil
			})
			var refusal model.ValidationError
			if got := errors.As(err, &refusal); got != tc.refused {
				t.Errorf("RejectDetachingParentEdge() error = %v; refused = %v, want %v", err, got, tc.refused)
			}
		})
	}
	for _, relType := range []model.RelationType{model.RelBlockedBy, model.RelRelatedTo} {
		err := RejectDetachingParentEdge(relType, "c", func() (model.IssueType, int, error) {
			t.Errorf("RejectDetachingParentEdge(%s) read the child; an edge of another type names no parent", relType)
			return model.TypeTask, 0, nil
		})
		if err != nil {
			t.Errorf("RejectDetachingParentEdge(%s) error = %v, want nil", relType, err)
		}
	}
	readFailed := errors.New("read failed")
	if err := RejectDetachingParentEdge(model.RelParentChild, "c", func() (model.IssueType, int, error) {
		return "", 0, readFailed
	}); !errors.Is(err, readFailed) {
		t.Errorf("RejectDetachingParentEdge() error = %v, want the read's error", err)
	}
}
