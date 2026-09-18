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
// [LAW:one-source-of-truth] This sentence was written out in three files, where
// three copies of one fact could drift. This is its one home.
// [LAW:types-are-the-program] It exists so the CLI's reason and exit-code sinks
// dispatch on the condition itself; a bare fmt.Errorf carried no type, so those
// sinks could not recognize a terminal precondition and told the agent to retry
// it forever (links-cli-errors-yfbg).
var ErrWorkspaceNotInitialized = errors.New("repository not initialized with lit — run 'lit init' first")

// requireInitializedWorkspace classifies one stat of the workspace root into
// the two answers its callers act on differently: `lit init` has never run
// here, or the stat itself failed for a reason the operator has to see.
//
// It takes the root and nothing else, deliberately. An earlier shape took any
// directory plus a label for the error text, and the journal lock pointed it at
// <root>/links/.dolt/noms — several levels below the fact being asserted. An
// absent directory down there is also what a damaged or half-deleted Dolt tree
// looks like, so that call answered "never initialized" for a workspace that
// plainly existed, and sent the caller to `lit init`, which refuses a root it
// cannot read. [LAW:types-are-the-program] With no directory to choose and no
// label to vary, that call is no longer expressible.
//
// It hands back a sentinel rather than a proof-carrying type because no type
// could honestly carry this proof — the directory can vanish between this stat
// and the caller's next syscall, so "initialized" is a fact about an instant,
// not about a value. [LAW:parse-dont-validate] What it does keep is the single
// boundary: the OS answer is classified once, here, instead of at each caller.
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
