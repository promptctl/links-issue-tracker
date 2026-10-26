package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"golang.org/x/sync/errgroup"

	"github.com/promptctl/links-issue-tracker/internal/environ"
)

// Discovery is what one scan found and what finding it cost: the stores, the
// directories the walk visited, and the working trees git was asked about.
type Discovery struct {
	// Locations are the distinct stores, sorted by StorageDir.
	Locations []Location
	// Directories is how many directories the walk visited.
	Directories int
	// WorkingTrees is how many working-tree roots the walk reached, one git
	// query each.
	WorkingTrees int
}

// Discover walks each root and returns the deduplicated set of lit store
// Locations beneath them, sorted by StorageDir. A directory contributes a
// Location when it is a git working tree whose derived store database exists;
// git repositories that were never `lit init`ed, and ordinary directories, are
// skipped. Multiple worktrees of one repository share one git-common-dir and so
// collapse to a single Location.
//
// The walk does not descend into a working tree: what is inside one belongs to
// its repository (source, node_modules, build output, vendored checkouts), so
// the scan costs the directories outside working trees, not every file under
// the root. A store whose repository sits inside another repository's working
// tree is found by naming its directory as a root.
//
// This is the discovery half of the cross-project seam: it turns roots into the
// value — a Discovery — that aggregation consumes. It opens no store and reads
// no ticket data; every Location it returns is openable read-only by its
// DatabasePath. [LAW:effects-at-boundaries] The filesystem walk and per-repo git
// queries are the only effects; the result is a plain value.
func Discover(env environ.Env, roots []string) (Discovery, error) {
	trees, directories, walkErr := workingTrees(roots)
	// [LAW:single-enforcer] Each tree's store is derived by Locate, the one
	// place lit asks git where a checkout is; the queries are independent, so
	// they run side by side, a core each.
	found := make([]treeStore, len(trees))
	var g errgroup.Group
	g.SetLimit(runtime.GOMAXPROCS(0))
	for i, tree := range trees {
		g.Go(func() error {
			found[i] = storeAt(env, tree)
			return nil
		})
	}
	_ = g.Wait() // every task returns nil; its error lands in found
	// The error a scan reports is the first in walk order, so it is the same
	// on every run: a tree's failed derivation comes before the walk error
	// the walk stopped at, since the walk reached the tree first.
	byStore := map[string]Location{}
	for _, f := range found {
		if f.err != nil {
			return Discovery{}, f.err
		}
		if f.holdsStore {
			byStore[f.loc.StorageDir] = f.loc
		}
	}
	if walkErr != nil {
		return Discovery{}, walkErr
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
	return Discovery{Locations: locations, Directories: directories, WorkingTrees: len(trees)}, nil
}

// workingTrees walks each root in turn and returns, in walk order, every
// working-tree root it reaches and how many directories it visited. A `.git`
// entry — directory for a main worktree, file for a linked one — marks a
// working-tree root, and the walk goes no deeper there. It stops at the first
// directory it cannot read, returning what it reached before it.
func workingTrees(roots []string) (trees []string, directories int, err error) {
	for _, root := range roots {
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// [LAW:no-silent-failure] A directory the scan cannot read is a
				// real condition the operator needs to see, not a store silently
				// missed.
				return fmt.Errorf("scan %q: %w", path, err)
			}
			if !d.IsDir() {
				return nil
			}
			directories++
			gitEntry := filepath.Join(path, ".git")
			if _, statErr := os.Lstat(gitEntry); statErr != nil {
				// [LAW:no-silent-failure] os.ErrNotExist is the ordinary "not a
				// repository root" case for nearly every directory; any other
				// stat error (EACCES, EIO, stale handle) would otherwise make a
				// real repository silently invisible, so it surfaces with
				// context — matching the store-database stat in storeAt.
				if errors.Is(statErr, os.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("stat %q: %w", gitEntry, statErr)
			}
			trees = append(trees, path)
			return filepath.SkipDir
		})
		if err != nil {
			return trees, directories, err
		}
	}
	return trees, directories, nil
}

// treeStore is what one working tree holds: its store's Location when
// holdsStore, or the error deriving it.
type treeStore struct {
	loc        Location
	holdsStore bool
	err        error
}

// storeAt derives the store a working tree's repository would hold and reports
// whether its database is there.
func storeAt(env environ.Env, tree string) treeStore {
	loc, err := Locate(env, tree)
	if err != nil {
		// A `.git` git itself refuses (a dangling gitfile, a non-repository)
		// is simply not a discoverable store — the expected "this looked like
		// a repo but isn't" outcome of a scan, not a failure of it. Any other
		// derivation error (e.g. a broken symlink under the common dir) is a
		// real problem and surfaces. [LAW:no-silent-failure]
		if errors.Is(err, ErrNotGitRepo) {
			return treeStore{}
		}
		return treeStore{err: err}
	}
	info, err := os.Stat(loc.DatabasePath)
	if err != nil {
		// [LAW:single-enforcer] "Is there a lit store here" is answered by the
		// same existence check OpenForRead uses — the store database dir — so
		// every discovered Location is openable and no lit-less git repo slips
		// in. os.ErrNotExist is the ordinary "git repo, no lit store" case;
		// any other stat error is surfaced, not swallowed.
		if errors.Is(err, os.ErrNotExist) {
			return treeStore{}
		}
		return treeStore{err: fmt.Errorf("stat store database %q: %w", loc.DatabasePath, err)}
	}
	// [LAW:types-are-the-program] A dolt store is a directory; a regular file
	// at that path (a leftover from a failed operation) exists but is not
	// openable, so counting it would break this function's contract that every
	// returned Location opens read-only. The predicate is "a store database
	// directory is here", not merely "a path exists here".
	return treeStore{loc: loc, holdsStore: info.IsDir()}
}
