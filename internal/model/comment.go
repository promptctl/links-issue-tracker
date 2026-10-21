package model

import "strings"

// CommentBody is a comment's text as it is stored: trimmed, and never blank.
type CommentBody string

// ParseCommentBody is the gate a comment's text passes through on its way in.
// A caller parses the body once, before it touches any ticket, so a blank body
// is one refusal of the invocation, however many tickets it named.
// [LAW:parse-dont-validate] [LAW:single-enforcer]
func ParseCommentBody(raw string) (CommentBody, error) {
	body := strings.TrimSpace(raw)
	if body == "" {
		return "", ValidationError{Message: "comment body is required"}
	}
	return CommentBody(body), nil
}
