package cli

// The typed error-definition taxonomy of the shared command surface: each type
// carries a machine-dispatchable classification constructed by handlers at the
// point of failure. The sinks are exit.go (ExitCode) and error_output.go
// (reason/remediation), which dispatch on these types via errors.As; no sink
// inspects message text. Feature-specific errors (e.g. SyncFailureError) live
// with their features.
//
// [LAW:decomposition] Split out of cli.go (links-store-mb6e.6) so the
// flag-parsing framework, the business command handlers, and the typed error
// taxonomy no longer grow in one file.

import "fmt"

type MergeConflictError struct {
	Message string
}

func (e MergeConflictError) Error() string {
	return e.Message
}

type CorruptionError struct {
	Message string
}

func (e CorruptionError) Error() string { return e.Message }

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

// ValidationError signals that a user-supplied value failed a domain constraint check.
type ValidationError struct {
	Message string
}

func (e ValidationError) Error() string { return e.Message }

// UnsupportedError signals use of a removed or unsupported feature.
// Feature names the unsupported capability (e.g. "--output") for targeted remediation.
type UnsupportedError struct {
	Message string
	Feature string
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
// (-h/--help as the first argument). It is an answer, not a failure: like
// pflag.ErrHelp it travels the error channel only because that is the channel
// resolve has, and Run — the seam that owns stdout — renders the carried usage
// and maps it to success before any error sink sees it.
// [LAW:effects-at-boundaries] resolve stays pure; the type carries the
// description of the help action outward to the edge that performs it.
type HelpRequestedError struct {
	Usage string
}

func (e HelpRequestedError) Error() string { return e.Usage }

// OutsideWorkspaceError signals that the command requires a git repository context.
type OutsideWorkspaceError struct {
	Message string
}

func (e OutsideWorkspaceError) Error() string { return e.Message }
