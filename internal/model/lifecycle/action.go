package lifecycle

// The lifecycle action is a sealed sum: one variant per action, each carrying
// exactly its payload. Start carries the new owner; Close carries the outcome
// (why the work was not finished); every other action carries nothing.
//
// [LAW:types-are-the-program] An assignee on close, a redirect target on a
// terminal outcome, or a resolution on reopen are unconstructible — the
// variant has no field to put them in — so the runtime carve-outs that used to
// reject those combinations do not exist. ActionName survives only as the
// persisted event-verb encoding (Name()); the sum itself never reaches disk.

// Action is the sealed lifecycle-action sum. Its only implementations are the
// eight variants below; isAction seals the set to this package.
type Action interface {
	// Name is the persisted event verb — the events-table encoding stays a
	// string; the variant exists only in memory.
	Name() ActionName
	isAction()
}

// StatusAction is the subset of actions that drive the status state machine,
// and Target is the one forward action→state map the machine consumes.
// Retention actions (archive/unarchive/delete/restore) act on the orthogonal
// Retention axis and have no status target, so they are deliberately not
// StatusActions — "apply a retention action to the status machine" is
// unrepresentable rather than runtime-rejected. [LAW:one-source-of-truth]
type StatusAction interface {
	Action
	Target() State
}

// Start claims the issue: it is the only action that rewrites the assignee,
// so it is the only variant that carries one.
type Start struct{ Assignee string }

func (Start) Name() ActionName { return ActionStart }
func (Start) Target() State    { return InProgress }
func (Start) isAction()        {}

// Done is the neutral success close: the work finished, no resolution recorded.
type Done struct{}

func (Done) Name() ActionName { return ActionDone }
func (Done) Target() State    { return Closed }
func (Done) isAction()        {}

// Close ends the work unfinished; the Outcome says why, and carries the
// redirect target exactly when the outcome redirects.
type Close struct{ Outcome Outcome }

func (Close) Name() ActionName { return ActionClose }
func (Close) Target() State    { return Closed }
func (Close) isAction()        {}

type Reopen struct{}

func (Reopen) Name() ActionName { return ActionReopen }
func (Reopen) Target() State    { return Open }
func (Reopen) isAction()        {}

type Archive struct{}

func (Archive) Name() ActionName { return ActionArchive }
func (Archive) isAction()        {}

type Unarchive struct{}

func (Unarchive) Name() ActionName { return ActionUnarchive }
func (Unarchive) isAction()        {}

type Delete struct{}

func (Delete) Name() ActionName { return ActionDelete }
func (Delete) isAction()        {}

type Restore struct{}

func (Restore) Name() ActionName { return ActionRestore }
func (Restore) isAction()        {}

// Outcome is the sealed close reason a Close action carries. The redirecting
// outcomes (Duplicate, Superseded) each carry the canonical ticket they
// redirect to; the terminal outcomes (Obsolete, Wontfix) carry nothing, so a
// redirect target on them cannot be expressed. The target travels through the
// status machine into the closed leaf and persists as its own column beside
// resolution, so write and read are projections of this one shape — there is
// no read-time re-derivation to keep in agreement.
type Outcome interface {
	// Resolution is the persisted column encoding of this outcome.
	Resolution() Resolution
	isOutcome()
}

// Duplicate closes the issue as a duplicate of the canonical ticket Of.
type Duplicate struct{ Of string }

func (Duplicate) Resolution() Resolution { return ResolutionDuplicate }
func (Duplicate) isOutcome()             {}

// Superseded closes the issue in favor of the ticket By that replaced it.
type Superseded struct{ By string }

func (Superseded) Resolution() Resolution { return ResolutionSuperseded }
func (Superseded) isOutcome()             {}

type Obsolete struct{}

func (Obsolete) Resolution() Resolution { return ResolutionObsolete }
func (Obsolete) isOutcome()             {}

type Wontfix struct{}

func (Wontfix) Resolution() Resolution { return ResolutionWontfix }
func (Wontfix) isOutcome()             {}
