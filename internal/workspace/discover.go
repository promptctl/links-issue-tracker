package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Discover walks each root recursively and returns the deduplicated set of lit
// store Locations beneath them, sorted by StorageDir. A directory contributes a
// Location when it is a git working tree whose derived store database exists;
// git repositories that were never `lit init`ed, and ordinary directories, are
// skipped. Multiple worktrees of one repository share one git-common-dir and so
// collapse to a single Location.
//
// This is the discovery half of the cross-project seam: it turns roots into the
// value — a []Location — that aggregation consumes. It opens no store and reads
// no ticket data; every Location it returns is openable read-only by its
// DatabasePath. [LAW:effects-at-boundaries] The filesystem walk and per-repo git
// queries are the only effects; the result is a plain value.
func Discover(roots []string) ([]Location, error) {
	byStore := map[string]Location{}
	for _, root := range roots {
		if err := discoverUnder(root, byStore); err != nil {
			return nil, err
		}
	}
	locations := make([]Location, 0, len(byStore))
	for _, loc := range byStore {
		locations = append(locations, loc)
	}
	// [LAW:verifiable-goals] Deterministic order makes the output a stable,
	// checkable value regardless of filesystem walk or map iteration order.
	sort.Slice(locations, func(i, j int) bool {
		return locations[i].StorageDir < locations[j].StorageDir
	})
	return locations, nil
}

// discoverUnder walks one root, folding every discovered store into byStore
// keyed by canonical StorageDir so worktrees of one repository dedup to a single
// entry. [LAW:no-shared-mutable-globals] byStore is owned by the calling
// Discover and passed in explicitly; discoverUnder never reaches for ambient
// state.
func discoverUnder(root string, byStore map[string]Location) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// [LAW:no-silent-failure] A directory the scan cannot read is a real
			// condition the operator needs to see, not a store silently missed.
			return fmt.Errorf("scan %q: %w", path, err)
		}
		// A repository's own .git tree holds no nested working trees; descending
		// into it would only waste git invocations. Skipping it by name still
		// lets the walk find genuinely nested repositories elsewhere in the tree.
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		// A `.git` entry — directory for a main worktree, file for a linked one —
		// marks a working-tree root and nowhere else, so git runs only at real
		// repository roots. [LAW:effects-at-boundaries]
		gitEntry := filepath.Join(path, ".git")
		if _, statErr := os.Lstat(gitEntry); statErr != nil {
			// [LAW:no-silent-failure] os.ErrNotExist is the ordinary "not a
			// repository root" case for nearly every directory; any other stat
			// error (EACCES, EIO, stale handle) would otherwise make a real
			// repository silently invisible, so it surfaces with context —
			// matching the store-database stat below.
			if errors.Is(statErr, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("stat %q: %w", gitEntry, statErr)
		}
		loc, derr := deriveLocation(path)
		if derr != nil {
			// A `.git` git itself refuses (a dangling gitfile, a non-repository)
			// is simply not a discoverable store — the expected "this looked like
			// a repo but isn't" outcome of a scan, not a failure of it. Any other
			// derivation error (e.g. a broken symlink under the common dir) is a
			// real problem and surfaces. [LAW:no-silent-failure]
			if errors.Is(derr, ErrNotGitRepo) {
				return nil
			}
			return derr
		}
		info, statErr := os.Stat(loc.DatabasePath)
		if statErr != nil {
			// [LAW:single-enforcer] "Is there a lit store here" is answered by the
			// same existence check OpenForRead uses — the store database dir — so
			// every discovered Location is openable and no lit-less git repo slips
			// in. os.ErrNotExist is the ordinary "git repo, no lit store" case;
			// any other stat error is surfaced, not swallowed.
			if errors.Is(statErr, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("stat store database %q: %w", loc.DatabasePath, statErr)
		}
		// [LAW:types-are-the-program] A dolt store is a directory; a regular file
		// at that path (a leftover from a failed operation) exists but is not
		// openable, so counting it would break this function's contract that every
		// returned Location opens read-only. The predicate is "a store database
		// directory is here", not merely "a path exists here".
		if !info.IsDir() {
			return nil
		}
		byStore[loc.StorageDir] = loc
		return nil
	})
}
