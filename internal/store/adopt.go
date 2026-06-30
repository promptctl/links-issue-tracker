package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
)

// LocalHasTickets reports whether an initialized store at doltRootDir holds any
// local issues. A not-yet-initialized store (no database on disk) has none, so
// it returns (false, nil): init's adopt gate treats "nothing to lose" uniformly
// whether the store is absent or merely empty, and — crucially — answers the
// question WITHOUT creating the store, so a fresh init can clone straight into
// the target path. [LAW:no-defensive-null-guards] absence is a real domain value
// (pristine workspace), matched here rather than papered over.
func LocalHasTickets(ctx context.Context, doltRootDir, workspaceID string) (bool, error) {
	cleanRoot, err := validateDoltRootDir(doltRootDir)
	if err != nil {
		return false, err
	}
	if !dirExists(filepath.Join(cleanRoot, doltDatabaseName)) {
		return false, nil
	}
	s, err := OpenForRead(ctx, cleanRoot, workspaceID)
	if err != nil {
		return false, err
	}
	defer s.Close()
	count, err := s.LocalIssueCount(ctx)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// AdoptRemoteByClone bootstraps the local store by CLONING the remote's history
// wholesale, writing it directly into doltRootDir as the database's first
// on-disk state. It is the medium-appropriate transfer for a git-backed remote,
// and the reason init no longer fetches to adopt.
//
// [FRAMING:representation] On a git-backed remote the Dolt table files live
// inside git blob objects, which have no random access: dolt serves a ranged
// read by streaming the whole blob and discarding the prefix (see dolt's
// gitblobstore). The FETCH path (DOLT_FETCH -> PullChunkTracker) reads the
// remote with many small ranged reads — the access pattern a *local* table file
// is built for — so every chunk read re-inflates the entire archive blob,
// turning a 38MB adopt into 20+ minutes of CPU. The CLONE path copies each
// archive table file as a whole blob exactly once (one sequential read per
// blob), then indexes locally — the only access pattern git blobs are good at.
// Adopt is semantically a clone (an empty store taking the remote's whole
// current state), so it uses the clone primitive. [LAW:decomposition]
//
// Cloning straight into the canonical path (rather than staging + swapping)
// keeps the dolt in-process singleton chunk-store cache honest: the target
// path's FIRST open is the clone, so the cache holds the cloned data — a swap
// under an already-opened path would leave the cache pointing at the pre-swap
// (empty) store. The caller must therefore NOT open the store before calling
// this (init's adopt decision is made from git signals alone for a fresh store).
// A non-empty target left by a prior failed adopt is discarded first — the
// caller has already verified it holds no local tickets via LocalHasTickets.
// [LAW:no-silent-failure] every failure is returned, never swallowed.
func AdoptRemoteByClone(ctx context.Context, doltRootDir, workspaceID, remoteName, remoteURL, branch string) (err error) {
	cleanRoot, err := validateDoltRootDir(doltRootDir)
	if err != nil {
		return err
	}
	if strings.TrimSpace(workspaceID) == "" {
		return errors.New("workspace id is required")
	}
	remoteName = strings.TrimSpace(remoteName)
	remoteURL = strings.TrimSpace(remoteURL)
	branch = strings.TrimSpace(branch)
	if remoteName == "" || remoteURL == "" || branch == "" {
		return fmt.Errorf("adopt by clone requires a remote name, url, and branch (got name=%q url=%q branch=%q)", remoteName, remoteURL, branch)
	}

	// The server root must exist before the workspace lock (whose file is a
	// sibling) can be taken and before the clone engine opens against it.
	if err := os.MkdirAll(cleanRoot, 0o755); err != nil {
		return fmt.Errorf("create dolt root dir: %w", err)
	}
	release, err := LockWorkspaceExclusive(ctx, cleanRoot)
	if err != nil {
		return err
	}
	defer func() {
		if relErr := release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()

	dbDir := filepath.Join(cleanRoot, doltDatabaseName)
	if dirExists(dbDir) {
		// A prior bootstrap or failed adopt left an empty database (the caller
		// verified no local tickets). Discard it so DOLT_CLONE — which refuses an
		// existing target — can recreate it, evicting any cached handle first so
		// the clone is read fresh rather than from the stale empty store.
		evictSingleton(dbDir)
		if err := os.RemoveAll(dbDir); err != nil {
			return fmt.Errorf("remove empty database before adopt: %w", err)
		}
	}

	if err := cloneRemoteDatabase(ctx, cleanRoot, workspaceID, remoteName, remoteURL, branch); err != nil {
		return err
	}
	if !dirExists(dbDir) {
		return fmt.Errorf("clone of remote %q produced no %q database", remoteName, doltDatabaseName)
	}
	return nil
}

// cloneRemoteDatabase opens an embedded Dolt engine rooted at serverRoot (with
// no current database) and clones remoteURL into the canonical database name via
// DOLT_CLONE, which copies whole archive table files in bulk. The git-backed
// remote defaults to the refs/dolt/data ref — the same ref lit's sync push
// writes — so no explicit ref is needed.
func cloneRemoteDatabase(ctx context.Context, serverRoot, workspaceID, remoteName, remoteURL, branch string) error {
	db, err := sql.Open(doltDriverName, buildDoltDSN(serverRoot, workspaceID, false))
	if err != nil {
		return fmt.Errorf("open dolt for clone: %w", err)
	}
	defer db.Close()
	if _, err := callIntProcedure(ctx, db, "DOLT_CLONE",
		"--remote", remoteName, "--branch", branch, remoteURL, doltDatabaseName); err != nil {
		return fmt.Errorf("clone remote %q (%s) branch %q: %w", remoteName, remoteURL, branch, err)
	}
	return nil
}

// evictSingleton drops dolt's in-process singleton chunk-store cache entries for
// the database at dbDir (the live store and its stats sidecar), closing the
// underlying stores. Best-effort: a missing entry is a no-op, and a close error
// here would only mask the adopt that follows, so it is intentionally not
// surfaced — the subsequent clone is what must succeed. The key form mirrors
// dolt's own DeleteFromSingletonCache callers (`<dbloc>/.dolt/noms`).
func evictSingleton(dbDir string) {
	_ = dbfactory.DeleteFromSingletonCache(filepath.ToSlash(filepath.Join(dbDir, ".dolt", "noms")), true)
	_ = dbfactory.DeleteFromSingletonCache(filepath.ToSlash(filepath.Join(dbDir, ".dolt", "stats", ".dolt", "noms")), true)
}
