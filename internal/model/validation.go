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

// RefusesCommand marks ValidationError as a Refusal.
func (ValidationError) RefusesCommand() {}

// Refusal is every error that refuses the command as issued: the same command
// against the same data is refused the same way on every run, and the message
// names what to change. The CLI maps it to exit 3 and to advice to adjust the
// command, never to retry. A refusal type that carries its own fields opts in
// by declaring the method beside itself, so it is classified where it is
// defined rather than by an arm every sink must remember. [LAW:single-enforcer]
type Refusal interface {
	error
	RefusesCommand()
}
