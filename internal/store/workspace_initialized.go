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

// requireInitializedDir classifies one stat of dir into the two answers its
// callers act on differently: this repository was never initialized, or the
// stat itself failed for a reason the operator has to see.
//
// It hands back a sentinel rather than a proof-carrying type because no type
// could honestly carry this proof — the directory can vanish between this stat
// and the caller's next syscall, so "initialized" is a fact about an instant,
// not about a value. [LAW:parse-dont-validate] What it does keep is the single
// boundary: the OS answer is classified once, here, instead of at each caller.
//
// label names the directory in the non-ENOENT wrap, because which directory
// failed to stat is the whole content of that error for an operator.
// [LAW:no-silent-failure] Only ENOENT means uninitialized; every other stat
// error (EACCES, EIO, ELOOP, …) is its own failure mode, never a guessed
// refusal and never a vague downstream Dolt-connection error.
func requireInitializedDir(dir string, label string) error {
	if _, statErr := os.Stat(dir); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return ErrWorkspaceNotInitialized
		}
		return fmt.Errorf("stat %s: %w", label, statErr)
	}
	return nil
}
