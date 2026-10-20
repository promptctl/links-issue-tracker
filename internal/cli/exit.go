package cli

const (
	ExitOK         = 0
	ExitGeneric    = 1
	ExitUsage      = 2
	ExitValidation = 3
	ExitNotFound   = 4
	ExitConflict   = 5
	// ExitNoWork: the command ran correctly and changed nothing — it had no
	// ticket to hand back, or the state it was asked for already held.
	// Distinct from ExitGeneric because a caller looping `lit next` has to tell
	// "stop, there is nothing for you" from "lit is broken", and under one code
	// its only way to do that was to parse the English — the thing every other
	// sink in this package exists to stop callers doing. The already-in-state
	// half is the same question asked of a mutation ("did I need to do
	// anything?") and gets the same answer rather than a code of its own.
	//
	// Not ExitOK: for `lit next`, 0 means "a ticket is on stdout". Exiting 0
	// with no row would hand the caller a success-shaped void.
	// [LAW:parse-dont-validate]
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
