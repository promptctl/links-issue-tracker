package model

import "github.com/promptctl/links-issue-tracker/internal/model/lifecycle"

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

// SchemaAhead is every error raised because a schema this binary was asked to
// operate on, the workspace's or the sync remote's, is past this binary's
// schema support. No change to the command and no rerun of this binary clears
// it: only a lit whose support reaches that schema can. So
// it is not a Refusal, whose advice is to adjust the command. The CLI maps it
// to exit 3 and to advice that leaves installing a different lit to the user.
// A type opts in by declaring the method beside itself. [LAW:single-enforcer]
type SchemaAhead interface {
	error
	SchemaAheadOfBinary()
}

// RecordedError is every error that carries what it decided and counted onto
// the failed command's event, as facts and counts under keys its type names.
// The record is declared beside the type, like its message, so the one sink
// that writes events reads it through this interface rather than through an
// arm per error type. [LAW:single-enforcer] [LAW:nothing-unseen]
type RecordedError interface {
	error
	EventRecord() (facts map[string]any, counts map[string]int)
}

// ErrorRecord is what a failed command's typed errors decided and counted, as
// event facts and counts, so it is countable without parsing the error's
// message. Each error type declares its own record (RecordedError);
// an error that declares none records nothing here. It merges the record of
// every error in the tree — each wrapped error and each branch of a join,
// depth-first in the order errors.As visits them — so a wrapper that records
// does not hide the facts of the error it wraps. A key two records both set
// keeps the first value: the outermost, earliest record wins, the precedence
// a single errors.As match gives. [LAW:nothing-unseen]
func ErrorRecord(err error) (facts map[string]any, counts map[string]int) {
	walkErrorTree(err, func(e error) {
		recorded, ok := e.(RecordedError)
		if !ok {
			return
		}
		f, c := recorded.EventRecord()
		facts = mergeFirstWins(facts, f)
		counts = mergeFirstWins(counts, c)
	})
	return facts, counts
}

// walkErrorTree visits err and every error it wraps, depth-first, in the order
// errors.As visits them.
func walkErrorTree(err error, visit func(error)) {
	if err == nil {
		return
	}
	visit(err)
	switch wrapper := err.(type) {
	case interface{ Unwrap() error }:
		walkErrorTree(wrapper.Unwrap(), visit)
	case interface{ Unwrap() []error }:
		for _, branch := range wrapper.Unwrap() {
			walkErrorTree(branch, visit)
		}
	}
}

// mergeFirstWins adds src's keys that dst does not already hold.
func mergeFirstWins[V any](dst, src map[string]V) map[string]V {
	for key, value := range src {
		if dst == nil {
			dst = make(map[string]V, len(src))
		}
		if _, held := dst[key]; !held {
			dst[key] = value
		}
	}
	return dst
}

// StateRequestError answers a command that asked for a state: Satisfied when
// the state already holds, so the command ran correctly and had nothing to do,
// and a refusal of the command as issued otherwise. The CLI maps the first to
// exit 6 with advice that nothing needs doing, the second to exit 3; neither
// ever to a retry. An epic's derived state, an issue's retention, and an edge
// the graph already holds (storage.RelationExistsError) all answer this way,
// and the one interface keeps their classification in one arm.
// [LAW:one-type-per-behavior]
type StateRequestError interface {
	error
	Satisfied() bool
}

var (
	_ StateRequestError = ContainerActionError{}
	_ StateRequestError = lifecycle.RetentionError{}
)
