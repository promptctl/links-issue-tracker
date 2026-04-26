package lifecycle

import "fmt"

type AllOf struct {
	Members []Lifecycle
}

func (a AllOf) Children() []Lifecycle {
	return append([]Lifecycle(nil), a.Members...)
}

func (a AllOf) State() State {
	progress := a.Progress()
	switch {
	case progress.Total > 0 && progress.Closed == progress.Total:
		return Closed
	case progress.InProgress > 0 || progress.Closed > 0:
		return InProgress
	default:
		return Open
	}
}

func (a AllOf) Progress() Progress {
	var out Progress
	// [LAW:dataflow-not-control-flow] Container progress is one recursive data fold over leaf lifecycle progress values, so future primitive leaves contribute without per-type branches.
	for _, progress := range Progresses(a) {
		out.Open += progress.Open
		out.InProgress += progress.InProgress
		out.Closed += progress.Closed
		out.Total += progress.Total
	}
	return out
}

func (a AllOf) AvailableActions() []ActionName {
	return nil
}

func (a AllOf) Apply(name ActionName, actor string, reason string) (Lifecycle, error) {
	return nil, fmt.Errorf("no %s action available on this issue", name)
}
