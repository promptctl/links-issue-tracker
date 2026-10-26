package store

import (
	"context"
	"fmt"
)

// The branch a session is on is per-connection state, and a rotated
// connection (reconnect, reopenEngine) opens on the database's default
// branch. So the Store records the branch a scratch user switched to, for as
// long as its scratch run lasts (runOnScratch), and every fresh connection
// resumes on it: a write a rotation retried inside a
// scratch run lands on the scratch branch, and one retried while landing it
// lands on the branch it lands on, never on whichever branch the connection
// happened to open on. Nothing records the branch a connection opens on, so
// no rename of it (ensureMasterDefaultBranch) can leave a stale copy, and
// activeBranch reads the connection itself. [LAW:one-source-of-truth]
// [LAW:no-ambient-temporal-coupling] [LAW:single-enforcer] checkout and
// checkoutNewAt are the only switches of a session's branch.

// checkout records branch, then switches the session to it. The switch's
// error is returned unwrapped, for the caller to name, and does not rotate the
// connection: rotation is retryTransientGCContention's, under its budget, and
// a caller that wants a failed switch recovered runs it inside that retry,
// whose rotation resumes on branch.
func (s *Store) checkout(ctx context.Context, branch string) error {
	s.checkedOut = branch
	return execProcedureDiscard(ctx, s.db, "DOLT_CHECKOUT", branch)
}

// checkoutNewAt creates branch at commit and switches the session to it, then
// records it; a branch of that name is reset to commit, the one the session is
// on included. It records only a branch that exists, since a creation that
// failed may have made none for a fresh connection to resume on; its retry
// makes it again. A failed switch is returned as checkout's is.
func (s *Store) checkoutNewAt(ctx context.Context, branch, commit string) error {
	if err := execProcedureDiscard(ctx, s.db, "DOLT_CHECKOUT", "-B", branch, commit); err != nil {
		return err
	}
	s.checkedOut = branch
	return nil
}

// resumeCheckedOut puts a fresh connection on the branch the session was last
// switched to. Before any switch, the branch the connection opened on is the
// session's branch, and there is nothing to resume.
func (s *Store) resumeCheckedOut(ctx context.Context) error {
	if s.checkedOut == "" {
		return nil
	}
	if err := execProcedureDiscard(ctx, s.db, "DOLT_CHECKOUT", s.checkedOut); err != nil {
		return fmt.Errorf("resume branch %q on the fresh connection: %w", s.checkedOut, err)
	}
	return nil
}
