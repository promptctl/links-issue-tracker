package store

import (
	"errors"
	"fmt"
	"os"
)

// ErrWorkspaceNotInitialized reports a repository where `lit init` has never
// run. It is a sentinel rather than a struct because nothing varies between its
// raise sites: the condition carries no data beyond itself, and the act it asks
// for is the same one every time.
//
// [LAW:one-source-of-truth] This sentence has one home, here, so no copy of it
// can drift.
// [LAW:types-are-the-program] It exists so the CLI's error classifier
// dispatches on the condition itself, matched by identity through errors.Is; a
// fresh fmt.Errorf at each raise site would have no identity to match, so the
// classifier could not recognize a terminal precondition and would tell the
// agent to retry it forever.
var ErrWorkspaceNotInitialized = errors.New("lit workspace is not initialized.  Run 'lit init' to initialize lit for this project.\nThis will pull the remote's existing backlog, if available")

// requireInitializedWorkspace classifies one stat of the workspace root into
// the two answers its callers act on differently: `lit init` has never run
// here, or the stat itself failed for a reason the operator has to see.
//
// It takes the root and nothing else, deliberately. An absent directory below
// the root is also what a damaged or half-deleted Dolt tree looks like, so a
// check pointed there would answer "never initialized" for a workspace that
// plainly exists, and send the caller to `lit init`, which refuses a root it
// cannot read.
//
// It hands back a sentinel rather than a proof-carrying type.
// [LAW:parse-dont-validate] exception: what keeps the answer true afterwards is
// the workspace lock each caller holds across the stat, so a proof that the
// workspace exists belongs to that lock's hold, not to this stat.
//
// [LAW:no-silent-failure] Only ENOENT means uninitialized; every other stat
// error (EACCES, EIO, ELOOP, …) is its own failure mode, never a guessed
// refusal and never a vague downstream Dolt-connection error.
func requireInitializedWorkspace(doltRootDir string) error {
	if _, statErr := os.Stat(doltRootDir); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return ErrWorkspaceNotInitialized
		}
		return fmt.Errorf("stat database dir: %w", statErr)
	}
	return nil
}
