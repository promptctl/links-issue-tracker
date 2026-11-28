package lifecycle

// State is the state of a container whose leaves have made progress p: closed
// once every leaf is, in progress once any leaf has started or closed, and
// open otherwise, a container with no leaves included. It is the container
// rule, whichever way the counts were reached. [LAW:one-source-of-truth]
func (p Progress) State() State {
	switch {
	case p.Total > 0 && p.Closed == p.Total:
		return Closed
	case p.InProgress > 0 || p.Closed > 0:
		return InProgress
	default:
		return Open
	}
}

// Tally is a container's lifecycle: the count of its leaves' states, however
// an engine counted them.
//
// Tally is deliberately not a StatusPrimitive: a container's state derives
// from its children, so there is no action to apply. The model dispatch
// boundary owns the rejection (and its wording), since it holds the issue
// identity and child-progress facts a useful message needs.
// [LAW:types-are-the-program] "Containers cannot be acted on" is structural —
// the type does not satisfy StatusPrimitive, so Apply cannot be handed one —
// not a runtime error string here.
type Tally struct {
	Leaves Progress
}

func (t Tally) State() State       { return t.Leaves.State() }
func (t Tally) Progress() Progress { return t.Leaves }
