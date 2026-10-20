package cli

// The typed error-definition taxonomy of the shared command surface: each type
// carries a machine-dispatchable classification constructed by handlers at the
// point of failure. One classifier dispatches on these types via errors.As,
// commandErrorReason (error_output.go), and the exit code and remediation both
// follow from the reason it returns; nothing inspects message text. Feature-specific errors (e.g. SyncFailureError) live
// with their features.
//
// [LAW:decomposition] The taxonomy has its own file so the flag-parsing
// framework, the business command handlers, and the typed error taxonomy do
// not grow in one file.

import "fmt"

type MergeConflictError struct {
	Message string
}

func (e MergeConflictError) Error() string {
	return e.Message
}

// CorruptionError is a store that failed an integrity check. Cause is the
// typed fault behind the failure when the check named one, so the reason and
// remediation follow the fault rather than the check that found it.
type CorruptionError struct {
	Message string
	Cause   error
}

func (e CorruptionError) Error() string { return e.Message }

func (e CorruptionError) Unwrap() error { return e.Cause }

// UsageError signals wrong CLI usage (bad argument count, unrecognised flag).
// [LAW:types-are-the-program] The type carries the ExitUsage classification so sinks dispatch on type.
type UsageError struct {
	Message string
}

func (e UsageError) Error() string { return e.Message }

// UnknownCommandError signals that the router received a command name it does not recognise.
type UnknownCommandError struct {
	Command string
}

func (e UnknownCommandError) Error() string { return fmt.Sprintf("unknown command %q", e.Command) }

// UnsupportedError signals use of a flag lit no longer accepts. Its message
// names what to use instead, so the type carries no more than the message.
type UnsupportedError struct {
	Message string
}

func (e UnsupportedError) Error() string { return e.Message }

// RetiredCommandError signals invocation of a command that has been retired from
// the presented surface. Retirement is deliberate and documented: the command
// still resolves (so the caller gets this pointer instead of cobra's bare
// "unknown command"), but it no longer runs — its intent now lives in the named
// replacement(s). [LAW:types-are-the-program] Retirement is its own typed reason
// with its own exit code and remediation, distinct from an unknown command (a
// name that never existed) and an unsupported flag.
type RetiredCommandError struct {
	Command     string // the retired command as invoked, e.g. "ready"
	Replacement string // guidance naming where the command's intent now lives
}

func (e RetiredCommandError) Error() string {
	return fmt.Sprintf("the %q command has been retired; %s", e.Command, e.Replacement)
}

// HelpRequestedError signals that the caller asked a command family for help
// (a first argument that parses as a true -h/--help, per isHelpFlag). It is an answer, not a failure: like
// pflag.ErrHelp it travels the error channel only because that is the channel
// resolve has, and Run — the seam that owns stdout — writes the carried page
// and maps it to success before any error sink sees it.
//
// Error composes that page rather than returning the usage line alone, so the
// description cannot be dropped by a caller that reaches for the error string:
// there is one composition, and every reader of this value gets all of it.
// [LAW:one-source-of-truth]
// [LAW:effects-at-boundaries] resolve stays pure; the type carries the
// description of the help action outward to the edge that performs it.
type HelpRequestedError struct {
	Usage string
	// Detail is the family's long-form description, rendered above Usage by the
	// same helpPage a leaf's flag help composes. Carrying it on the value keeps
	// a family's whole help answer in one place, the way the row-level
	// retirement pointer carries its whole answer rather than half of it.
	Detail string
}

func (e HelpRequestedError) Error() string { return helpPage(e.Detail, e.Usage) }

// OutsideWorkspaceError signals that the command requires a git repository context.
type OutsideWorkspaceError struct {
	Message string
}

func (e OutsideWorkspaceError) Error() string { return e.Message }
