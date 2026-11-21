package store

import (
	"errors"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
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

// requireInitializedWorkspace refuses a workspace where `lit init` has never
// run with ErrWorkspaceNotInitialized; whether it has is workspace.Initialized's
// answer, so every open of the store refuses exactly the workspaces every other
// open refuses. [LAW:one-source-of-truth]
//
// It hands back a sentinel rather than a proof-carrying type.
// [LAW:parse-dont-validate] exception: what keeps the answer true afterwards is
// the workspace lock each caller holds across the check, so a proof that the
// workspace exists belongs to that lock's hold, not to this check.
func requireInitializedWorkspace(doltRootDir string) error {
	initialized, err := workspace.Initialized(doltRootDir)
	if err != nil || initialized {
		return err
	}
	return ErrWorkspaceNotInitialized
}
