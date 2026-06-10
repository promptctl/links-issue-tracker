package lifecycle

// AllOf is deliberately not Actionable: a container's state derives from its
// children, so there is no action to apply. The model dispatch boundary owns
// the rejection (and its wording), since it holds the issue identity and
// child-progress facts a useful message needs.
// [LAW:types-are-the-program] "Containers cannot be acted on" is structural —
// the type does not satisfy Actionable — not a runtime error string here.
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
