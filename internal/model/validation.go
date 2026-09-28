package model

// ValidationError is a refusal of a value the caller supplied: it breaks a
// domain rule, so the same input is refused the same way on every run. The CLI
// maps it to exit 3 and to advice to change the input, never to retry.
//
// It lives here, beside the rules, so a rule is classified where it is written.
// A rule that raised an untyped error relied on every caller remembering to
// wrap it, and each caller that forgot handed a deterministic refusal to the
// "retry, then run lit doctor" default. [LAW:single-enforcer]
// [LAW:types-are-the-program] Callers dispatch on the type, never the text.
type ValidationError struct {
	Message string
}

func (e ValidationError) Error() string { return e.Message }
