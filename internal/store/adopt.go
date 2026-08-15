package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
)

// adoptPendingMarkerName is the durable adopt-lifecycle sentinel. It lives
// INSIDE the dolt root (a sibling of the database directory), not at the
// dirname position the lock files use, and the placement is the design:
// `lit snapshots restore` and the heal/promote recovery paths swap the dolt
// root wholesale, so a marker stored inside it travels with the directory
// whose validity it describes — restoring a good snapshot un-condemns the
// workspace by construction, and a snapshot taken of adopt residue stays
// condemned, with no second cleanup site anywhere. The locks sit outside for
// the inverse reason: they must SURVIVE the rotation to keep excluding
// concurrent access. [LAW:one-source-of-truth] the map is stored with its
// territory.
const adoptPendingMarkerName = ".links-adopt-pending"

// AdoptPendingMarkerPath returns the adopt-pending marker path for a Dolt
// root directory. [LAW:one-source-of-truth] One naming convention, same as
// WorkspaceLockPath/EngineLockPath; any callsite (including tests fabricating
// an abandoned adopt) reads the path from this function.
func AdoptPendingMarkerPath(databasePath string) string {
	return filepath.Join(filepath.Clean(databasePath), adoptPendingMarkerName)
}

// adoptPendingMarker records which adopt was in flight, purely to make the
// refusal diagnostic name the interrupted operation. PRESENCE of the file is
// the semantic; unreadable or garbage content still condemns the workspace.
type adoptPendingMarker struct {
	StartedAt string `json:"started_at"`
	Remote    string `json:"remote"`
	Branch    string `json:"branch"`
}

// errAdoptPending is the sentinel wrapped by every marker-present refusal, so
// LocalHasTickets can distinguish "condemned residue — nothing to lose" from
// a real I/O failure reading the marker.
var errAdoptPending = errors.New("adopt pending")

// writeAdoptPendingMarker durably records "a destructive adopt is in flight"
// and must complete BEFORE the first destructive act. The fsync is
// load-bearing: the marker's whole job is surviving a crash or an abandoned
// clone goroutine mid-write, and a marker sitting in the page cache when the
// power goes protects nothing. If the truth-teller cannot be made durable,
// the destructive window must not open. [LAW:no-silent-failure]
func writeAdoptPendingMarker(cleanRoot, remote, branch string, now time.Time) error {
	payload, err := json.Marshal(adoptPendingMarker{
		StartedAt: now.UTC().Format(time.RFC3339),
		Remote:    remote,
		Branch:    branch,
	})
	if err != nil {
		return fmt.Errorf("encode adopt-pending marker: %w", err)
	}
	path := AdoptPendingMarkerPath(cleanRoot)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("write adopt-pending marker: %w", err)
	}
	if _, err := f.Write(payload); err != nil {
		return errors.Join(fmt.Errorf("write adopt-pending marker: %w", err), f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync adopt-pending marker: %w", err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close adopt-pending marker: %w", err)
	}
	// [LAW:no-silent-failure] exception: the directory fsync that would make
	// the new dirent itself crash-durable is not supported on every platform
	// (Windows cannot fsync a directory handle), and there is no recovery
	// action when it fails — the file's own fsync above is the load-bearing
	// one, so a dirent-durability miss only narrows back to the pre-marker
	// crash window rather than corrupting anything.
	if dir, dirErr := os.Open(cleanRoot); dirErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// clearAdoptPendingMarker removes the marker; an already-absent marker is the
// success state, not an error.
func clearAdoptPendingMarker(cleanRoot string) error {
	if err := os.Remove(AdoptPendingMarkerPath(cleanRoot)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear adopt-pending marker: %w", err)
	}
	return nil
}

// requireNoPendingAdopt is the checkpoint that keeps an interrupted adopt's
// residue from ever being opened as a store. A failed-and-returned clone
// cleans up after itself, but the failure shapes that CANNOT return — init's
// deadline abandoning the clone goroutine mid-write, a crash, SIGKILL — leave
// whatever undefined partial state the clone had reached, and before this
// marker existed the only signal downstream was "the database directory
// exists", a map that reads residue as a valid store. [LAW:parse-dont-validate]
// presence of the marker is the (negative) stamp: directory existence alone
// is never again trusted as store validity.
//
// Only a provably-absent marker returns nil. A present marker returns the
// condemnation (wrapping errAdoptPending) whether or not its content parses —
// presence is the semantic — and a read failure other than ENOENT is its own
// loud error: an unreadable checkpoint must refuse, not wave through.
// [LAW:no-silent-failure]
func requireNoPendingAdopt(cleanRoot string) error {
	path := AdoptPendingMarkerPath(cleanRoot)
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read adopt-pending marker: %w", err)
	}
	interrupted := "a backlog adopt"
	var marker adoptPendingMarker
	if json.Unmarshal(payload, &marker) == nil && marker.Remote != "" && marker.Branch != "" {
		interrupted = fmt.Sprintf("the adopt of %s/%s started %s", marker.Remote, marker.Branch, marker.StartedAt)
	}
	return fmt.Errorf(
		"%w: a `lit init` backlog adopt was interrupted before completing (%s; marker %s), so the on-disk store "+
			"is that adopt's leftover partial state, not a usable backlog. Run `lit init` to retry: it discards the "+
			"leftover and re-clones the remote backlog. If the remote no longer carries the backlog, delete %s to "+
			"abandon the adopt and start fresh — nothing local-only exists under an interrupted adopt",
		errAdoptPending, interrupted, path, cleanRoot,
	)
}

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
	// An interrupted adopt's residue is "nothing to lose" BY CONSTRUCTION: the
	// marker is only ever written after this same gate confirmed the store
	// held no tickets, and no mutation can run while it stands (every normal
	// open refuses on it). Answering false here — without opening whatever
	// undefined partial state the clone left — is what lets the retrying
	// `lit init` discard the residue and re-clone instead of wedging on an
	// unopenable leftover. A marker read failure stays loud: it is the one
	// state where "safe to discard" genuinely cannot be confirmed.
	// [LAW:parse-dont-validate] [LAW:no-silent-failure]
	if err := requireNoPendingAdopt(cleanRoot); err != nil {
		if errors.Is(err, errAdoptPending) {
			return false, nil
		}
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
// A non-empty target left by a prior bootstrap or interrupted adopt is
// discarded first — the caller has already verified it holds no local tickets
// via LocalHasTickets (for interrupted-adopt residue, via its marker
// fast-path). [LAW:no-silent-failure] every failure is returned, never
// swallowed.
//
// Postcondition (two states, never three): a nil return means the database
// directory holds the complete cloned backlog and no adopt-pending marker
// remains; an error return means no partial database remains either — or,
// when even the cleanup could not complete, the durable marker remains to
// keep the leftover from ever being opened as a store.
// [LAW:types-are-the-program] "undefined partial state that looks valid" is
// unrepresentable at this seam.
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

	// The marker brackets the ENTIRE destructive window (discard + clone): it
	// is durably on disk before the first byte is at risk and leaves only by
	// the two removals below — after a validated clone, or after a failure
	// whose residue was fully discarded. Every other exit (crash, SIGKILL,
	// init's deadline abandoning the clone goroutine mid-write) leaves it in
	// place, so the NEXT process can tell "failed-adopt residue" from "real
	// store" instead of inferring validity from directory existence.
	// [LAW:no-ambient-temporal-coupling] the adopt lifecycle is durable state,
	// not an inference from the process having exited at the right moment.
	if err := writeAdoptPendingMarker(cleanRoot, remoteName, branch, time.Now()); err != nil {
		return err
	}

	dbDir := filepath.Join(cleanRoot, doltDatabaseName)
	if dirExists(dbDir) {
		// A prior bootstrap left an empty database, or an interrupted adopt
		// left partial residue (the caller's LocalHasTickets gate confirmed
		// "nothing to lose" either way — for residue, via the marker
		// fast-path). Discard it so DOLT_CLONE — which refuses an existing
		// target — can recreate it, evicting any cached handle first so the
		// clone is read fresh rather than from the stale store.
		evictSingleton(dbDir)
		if err := os.RemoveAll(dbDir); err != nil {
			return fmt.Errorf("remove database before adopt: %w", err)
		}
	}

	if cloneErr := cloneRemoteDatabase(ctx, cleanRoot, workspaceID, remoteName, remoteURL, branch); cloneErr != nil {
		// dolt's own procedure cleans its clone target on most failures, but
		// that is a fork-internal courtesy; lit owns its postcondition here: a
		// RETURNED failure leaves no partial database and no marker, so the
		// retry the error text asks for starts from a provably clean slate.
		// If the residue cannot be removed, the marker stays — it is the
		// truth-teller that keeps the leftover from ever being opened as a
		// store. [LAW:no-silent-failure] every cleanup miss rides the returned
		// error; nothing is swallowed.
		if dirExists(dbDir) {
			evictSingleton(dbDir)
			if rmErr := os.RemoveAll(dbDir); rmErr != nil {
				return errors.Join(cloneErr, fmt.Errorf("clean up partial clone: %w", rmErr))
			}
		}
		if clearErr := clearAdoptPendingMarker(cleanRoot); clearErr != nil {
			return errors.Join(cloneErr, clearErr)
		}
		return cloneErr
	}
	if !dirExists(dbDir) {
		// The clone claimed success but produced no database. The marker stays:
		// something is off enough that the workspace should remain condemned
		// until a retry provably completes.
		return fmt.Errorf("clone of remote %q produced no %q database", remoteName, doltDatabaseName)
	}
	// The last act: marker-present means "never fully adopted", so the marker
	// may only leave after the clone's product is validated above.
	return clearAdoptPendingMarker(cleanRoot)
}

// cloneRemoteDatabase opens an embedded Dolt engine rooted at serverRoot (with
// no current database) and clones remoteURL into the canonical database name via
// DOLT_CLONE, which copies whole archive table files in bulk. The git-backed
// remote defaults to the refs/dolt/data ref — the same ref lit's sync push
// writes — so no explicit ref is needed.
func cloneRemoteDatabase(ctx context.Context, serverRoot, workspaceID, remoteName, remoteURL, branch string) error {
	db, err := openDoltPool(serverRoot, workspaceID, "", engineWrite)
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

// evictSingleton drops dolt's in-process singleton chunk-store cache entries
// for the database at dbDir (the live store and its stats sidecar) WITHOUT
// closing them. lit's own opens bypass the cache entirely (see
// newDoltConnector), so any entry found here was left by a dolt-internal load
// path (e.g. during DOLT_CLONE) whose store the owning engine has already
// closed — closing the carcass a second time trips dolt's refcount assert on
// the archive readers the two instances shared. Dropping the entry is the
// whole job: it only exists to keep a stale cached handle from ever serving
// the re-adopted path. Best-effort: a missing entry is a no-op. The key form
// mirrors dolt's own DeleteFromSingletonCache callers (`<dbloc>/.dolt/noms`).
func evictSingleton(dbDir string) {
	_ = dbfactory.DeleteFromSingletonCache(filepath.ToSlash(filepath.Join(dbDir, ".dolt", "noms")), false)
	_ = dbfactory.DeleteFromSingletonCache(filepath.ToSlash(filepath.Join(dbDir, ".dolt", "stats", ".dolt", "noms")), false)
}
