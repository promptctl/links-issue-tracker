package model

import (
	"errors"
	"testing"
)

// A body is its text trimmed; a body with nothing left after the trim is
// refused as a validation error, whatever whitespace it was made of.
func TestParseCommentBodyTrimsAndRefusesBlank(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", " ", " \t\n", "  "} {
		_, err := ParseCommentBody(raw)
		var invalid ValidationError
		if !errors.As(err, &invalid) || invalid.Message != "comment body is required" {
			t.Errorf("ParseCommentBody(%q) error = %v, want comment body is required", raw, err)
		}
	}
	for raw, want := range map[string]string{
		"note":            "note",
		"  note \n":       "note",
		"a  b\n\nc":       "a  b\n\nc",
		"\t- item\n- two": "- item\n- two",
	} {
		body, err := ParseCommentBody(raw)
		if err != nil {
			t.Errorf("ParseCommentBody(%q) error = %v", raw, err)
			continue
		}
		if got, err := body.Text(); err != nil || got != want {
			t.Errorf("ParseCommentBody(%q).Text() = %q, %v, want %q", raw, got, err, want)
		}
	}
}

// The zero value was never parsed, so it holds no text, and reading it is the
// same refusal a blank body gets: no engine can store a comment from it.
func TestZeroCommentBodyIsRefused(t *testing.T) {
	t.Parallel()
	_, err := CommentBody{}.Text()
	var invalid ValidationError
	if !errors.As(err, &invalid) || invalid.Message != "comment body is required" {
		t.Errorf("CommentBody{}.Text() error = %v, want comment body is required", err)
	}
}
