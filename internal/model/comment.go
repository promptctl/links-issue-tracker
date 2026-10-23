package model

import "strings"

// CommentBody is a comment's text as a write takes it: trimmed, and never
// blank. Only ParseCommentBody builds one that holds text, so a body is parsed
// once, where it enters. The zero value holds none: Go lets any code write it
// without parsing, so Text tells it from a parsed body and refuses it rather
// than let a blank comment be stored. [LAW:parse-dont-validate]
// [LAW:no-silent-failure]
type CommentBody struct {
	text string
}

// ParseCommentBody is the gate a comment's text passes through on its way in.
// A caller parses the body once, before it touches any ticket, so a blank body
// is one refusal of the invocation, however many tickets it named.
// [LAW:single-enforcer]
func ParseCommentBody(raw string) (CommentBody, error) {
	body := strings.TrimSpace(raw)
	if body == "" {
		return CommentBody{}, errCommentBodyRequired
	}
	return CommentBody{text: body}, nil
}

// Text is the body's text, and the one way to read it: an engine storing a
// comment reads it here, so none can store the zero value's empty text.
func (b CommentBody) Text() (string, error) {
	if b.text == "" {
		return "", errCommentBodyRequired
	}
	return b.text, nil
}

var errCommentBodyRequired = ValidationError{Message: "comment body is required"}
