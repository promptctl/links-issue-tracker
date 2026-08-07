package workflows

// Dispatch is the one seam every curated command routes its occasions
// through: synchronous, in-process, called directly from the command path —
// no bus, no async queue, no subscriber list. [LAW:no-mode-explosion] There
// is exactly one Dispatch, not a registry other packages add themselves to.
//
// It has no behavior of its own yet. Matching definitions against the
// occasion and injecting their guidance, and recording the occasion for
// observability/tracing, are later tickets in the same epic; they extend
// this function's body directly in place rather than registering a handler
// with it. [LAW:single-enforcer] Wiring every command through this one call
// now means those tickets touch one function, not every call site again.
func Dispatch(Occasion) {}
