package lifecycle

import (
	"fmt"
	"strings"
)

// Phase is where an in-progress ticket's work stands. Only in_progress has
// phases: a start enters plan, and leaving in_progress ends the phase. The set
// is closed and ordered as the work runs: plan, impl, review, merge.
type Phase string

const (
	PhasePlan   Phase = "plan"
	PhaseImpl   Phase = "impl"
	PhaseReview Phase = "review"
	PhaseMerge  Phase = "merge"
)

// Phases lists every phase in work order. ParsePhase's admissible set and
// every listing of the phases derive from it. [LAW:one-source-of-truth]
func Phases() []Phase {
	return []Phase{PhasePlan, PhaseImpl, PhaseReview, PhaseMerge}
}

// ParsePhase maps an untrusted phase name into the sealed set.
// [LAW:single-enforcer] The only string-to-Phase gate.
func ParsePhase(value string) (Phase, error) {
	normalized := Phase(strings.TrimSpace(strings.ToLower(value)))
	for _, phase := range Phases() {
		if normalized == phase {
			return normalized, nil
		}
	}
	return "", fmt.Errorf("invalid phase %q (valid: %s)", value, phaseList())
}

func phaseList() string {
	names := make([]string, 0, len(Phases()))
	for _, phase := range Phases() {
		names = append(names, string(phase))
	}
	return strings.Join(names, ", ")
}

// SetPhase moves an in-progress ticket to Phase. It acts on neither the status
// machine nor retention: the ticket stays in_progress, and only its phase
// moves. [LAW:types-are-the-program]
type SetPhase struct{ Phase Phase }

func (SetPhase) Name() ActionName { return ActionPhase }
func (SetPhase) isAction()        {}
