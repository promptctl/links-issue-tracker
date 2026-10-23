package cli

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func commentBody(t *testing.T, raw string) model.CommentBody {
	t.Helper()
	body, err := model.ParseCommentBody(raw)
	if err != nil {
		t.Fatalf("model.ParseCommentBody(%q): %v", raw, err)
	}
	return body
}
