package storagetest

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// CommentBody parses raw as a comment's text, failing the test if it is blank.
func CommentBody(t testing.TB, raw string) model.CommentBody {
	t.Helper()
	body, err := model.ParseCommentBody(raw)
	if err != nil {
		t.Fatalf("model.ParseCommentBody(%q): %v", raw, err)
	}
	return body
}
