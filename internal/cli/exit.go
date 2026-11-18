package cli

const (
	ExitOK         = 0
	ExitGeneric    = 1
	ExitUsage      = 2
	ExitValidation = 3
	ExitNotFound   = 4
	ExitConflict   = 5
	// ExitNoWork: the command ran correctly and changed nothing, because the
	// state it was asked for already held. Distinct from ExitGeneric so a
	// caller can tell "nothing to do" from "lit is broken" without parsing the
	// English. [LAW:parse-dont-validate]
	ExitNoWork     = 6
	ExitCorruption = 7
)

// ExitCode is the exit code a command's error arrives on: the one its reason
// carries. [LAW:single-enforcer] commandErrorReason is the one classifier, so a
// failure's code and its reason cannot disagree.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	return commandErrorReason(err).exit
}
