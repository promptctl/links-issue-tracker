package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/dbsnapshot"
	"github.com/promptctl/links-issue-tracker/internal/environ"
)

// A store's git mirrors live outside the repository, in its mirror base under
// the mirror root: `<mirror root>/<16 hex of sha256(database dir)>/<key>/repo.git`.
// Git refuses a GIT_DIR longer than PATH_MAX-40, 220 characters on Windows,
// and a mirror inside the repository sits 118 to 152 characters below its
// root, so from a repository a few directories deep nothing could sync. The
// mirror root's path does not grow with the repository's depth.
//
// Each base holds a rootFileName file naming the database directory it
// serves. Its reader, readMirrorBases, is what both collectors of bases read:
// the take-time sweep of a clone base (SweepMirrorBasesUnder) and the prune of
// bases whose repository is gone (sweepOrphanedMirrorBases).

// MirrorRootEnvVar names a directory that replaces the default mirror root.
const MirrorRootEnvVar = "LIT_MIRROR_ROOT"

// rootFileName is the file in a mirror base naming the database directory the
// base serves. It is not a cache key, so the remote-cache prune never reads it
// as a mirror (isRemoteCacheKey).
const rootFileName = "root"

// mirrorBaseKeyLen is how many hex digits of sha256(database dir) name a base:
// 64 bits, against a machine's handful of repositories.
const mirrorBaseKeyLen = 16

// MirrorRootOf is the directory every store's mirror base lives under, for a
// run in env: MirrorRootEnvVar when set, else `<user cache dir>/lit/m`.
func MirrorRootOf(env environ.Env) (string, error) {
	if root := env.Get(MirrorRootEnvVar); root != "" {
		if !filepath.IsAbs(root) {
			return "", fmt.Errorf("%s=%q is not an absolute path", MirrorRootEnvVar, root)
		}
		return filepath.Clean(root), nil
	}
	cache, err := env.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate the git mirror root: %w (set %s to choose one)", err, MirrorRootEnvVar)
	}
	// Short, not the app directory config uses: this path is the prefix of
	// every mirror's GIT_DIR, which Windows caps (see above).
	return filepath.Join(cache, "lit", "m"), nil
}

// MirrorBase is where the mirrors of the store rooted at doltRootDir live,
// under mirrorRoot. [LAW:one-source-of-truth] every reader and writer of a
// store's mirrors gets the path from here.
func MirrorBase(mirrorRoot, doltRootDir string) string {
	return filepath.Join(mirrorRoot, mirrorBaseName(mirrorBaseIdentity(doltRootDir)))
}

// mirrorBaseName is the directory name of the base serving the database
// directory identity.
func mirrorBaseName(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])[:mirrorBaseKeyLen]
}

// mirrorBaseIdentity is the database directory a base serves, as its root
// file records it and its name hashes it.
func mirrorBaseIdentity(doltRootDir string) string {
	return filepath.Clean(gitCacheRootFor(doltRootDir))
}

// mirrorBaseOf is the base of the store rooted at doltRootDir, under
// mirrorRoot, with the database its root file names.
func mirrorBaseOf(mirrorRoot, doltRootDir string) mirrorBaseEntry {
	return mirrorBaseEntry{dir: MirrorBase(mirrorRoot, doltRootDir), database: mirrorBaseIdentity(doltRootDir)}
}

// ensureMirrorBase creates the base of the store rooted at doltRootDir, with
// its root file, before anything can write a mirror into it.
func ensureMirrorBase(mirrorRoot, doltRootDir string) (string, error) {
	base := mirrorBaseOf(mirrorRoot, doltRootDir)
	return base.dir, base.ensure()
}

// ensure creates the base with its root file. [LAW:single-enforcer] every
// producer of a base creates it here, so no base exists without the root file
// both collectors read: a base without one is never collected.
func (b mirrorBaseEntry) ensure() error {
	root := filepath.Join(b.dir, rootFileName)
	if recorded, err := os.ReadFile(root); err == nil && string(recorded) == b.database {
		return nil
	}
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return fmt.Errorf("create git mirror base %s: %w", b.dir, err)
	}
	if err := os.WriteFile(root, []byte(b.database), 0o644); err != nil {
		return fmt.Errorf("record the database a git mirror base serves (%s): %w", b.dir, err)
	}
	return nil
}

// removeCounting removes the base, its root file last, so a removal that
// fails partway leaves a base the next collection still finds and retries.
func (b mirrorBaseEntry) removeCounting() (freed int64, removed bool, err error) {
	entries, err := os.ReadDir(b.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	var failed []error
	for _, entry := range entries {
		if entry.Name() == rootFileName {
			continue
		}
		n, _, err := removeTreeCounting(filepath.Join(b.dir, entry.Name()))
		freed += n
		if err != nil {
			failed = append(failed, err)
		}
	}
	if len(failed) > 0 {
		return freed, false, errors.Join(failed...)
	}
	n, removed, err := removeTreeCounting(b.dir)
	return freed + n, removed, err
}

// mirrorBaseOpenedBy is the base Dolt opens a git remote of the store rooted
// at doltRootDir in, created with its root file. Dolt's callback has no error
// to return, so a failure to create it is left to surface where Dolt writes
// the mirror into the same directory, which fails the open with the
// filesystem's own error.
func mirrorBaseOpenedBy(mirrorRoot, doltRootDir string) string {
	base, err := ensureMirrorBase(mirrorRoot, doltRootDir)
	if err != nil {
		return MirrorBase(mirrorRoot, doltRootDir)
	}
	return base
}

// SeedCloneMirrors copies the mirrors in the base of the live store rooted at
// liveRoot into the base of the clone rooted at cloneRoot. A clone takes it
// beside its copy of the Dolt directory, under the same hold, so the clone
// fetches only what the live mirrors lack, as it did when the mirrors lived
// inside the copied tree. The clone's base is created with its own root file
// before any mirror is copied into it, so a copy cut short is collected with
// the clone. A live store that has opened no git remote has no base, and the
// clone starts with none.
func SeedCloneMirrors(ctx context.Context, mirrorRoot, liveRoot, cloneRoot string) error {
	live := MirrorBase(mirrorRoot, liveRoot)
	entries, err := os.ReadDir(live)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("read the live git mirror base %s: %w", live, err)
	}
	clone, err := ensureMirrorBase(mirrorRoot, cloneRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == rootFileName {
			continue
		}
		if err := dbsnapshot.CloneTree(ctx, filepath.Join(live, entry.Name()), filepath.Join(clone, entry.Name())); err != nil {
			return fmt.Errorf("copy the live git mirror %s to the clone's base %s: %w", entry.Name(), clone, err)
		}
	}
	return nil
}

// RemoveCloneMirrors removes the mirror base of the clone rooted at
// cloneRoot. A clone's removal calls it with the removal of the clone's tree,
// so no mirror outlives the clone that made it.
func RemoveCloneMirrors(mirrorRoot, cloneRoot string) error {
	base := mirrorBaseOf(mirrorRoot, cloneRoot)
	if _, _, err := base.removeCounting(); err != nil {
		return fmt.Errorf("remove the clone's git mirror base %s: %w", base.dir, err)
	}
	return nil
}

// mirrorBaseEntry is one base under a mirror root, with the database
// directory its root file names.
type mirrorBaseEntry struct {
	dir      string
	database string
}

// readMirrorBases lists the bases under mirrorRoot that carry a root file. A
// directory without one is not a base this package wrote, so no collector
// owns it. A mirror root that does not exist holds no bases. A base whose
// root file cannot be read is left out and named in unreadable, so one base
// of any repository on the machine never stops the collection of the rest.
func readMirrorBases(mirrorRoot string) (bases []mirrorBaseEntry, unreadable error, err error) {
	entries, err := os.ReadDir(mirrorRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read git mirror root %s: %w", mirrorRoot, err)
	}
	var skipped []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(mirrorRoot, entry.Name())
		recorded, err := os.ReadFile(filepath.Join(dir, rootFileName))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			skipped = append(skipped, fmt.Errorf("read git mirror base %s: %w", dir, err))
			continue
		}
		// A base is named by the database its root file records. A directory
		// whose name does not match is not one this package wrote, whatever
		// it holds, so no collector removes it: a mirror root shared with
		// other data never loses a byte of it.
		if mirrorBaseName(string(recorded)) != entry.Name() {
			continue
		}
		bases = append(bases, mirrorBaseEntry{dir: dir, database: string(recorded)})
	}
	return bases, errors.Join(skipped...), nil
}

// isUnder reports whether path is dir or lies below it, both cleaned.
func isUnder(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// SweepMirrorBasesUnder removes every base under mirrorRoot whose database
// lies under dir. A clone's take calls it on the clone base before it removes
// that base, so the mirrors of a clone a killed cycle left behind go with the
// clone itself, on the same schedule. A base whose root file cannot be read
// may serve any repository on the machine, so it does not fail the take; the
// prune reports it.
func SweepMirrorBasesUnder(mirrorRoot, dir string) error {
	bases, _, err := readMirrorBases(mirrorRoot)
	if err != nil {
		return err
	}
	var failed []error
	for _, base := range bases {
		if !isUnder(base.database, dir) {
			continue
		}
		if _, _, err := base.removeCounting(); err != nil {
			failed = append(failed, fmt.Errorf("remove git mirror base %s of %s: %w", base.dir, base.database, err))
		}
	}
	return errors.Join(failed...)
}

// sweepOrphanedMirrorBases removes every base under mirrorRoot whose Dolt
// root no longer exists: the repository it served was deleted. It reads the
// Dolt root, not the database directory, because an adopt sets the database
// directory aside and clones a new one into its place while the mirror is in
// use, and the Dolt root stands throughout. It reports what it removed as the
// remote-cache prune does.
func sweepOrphanedMirrorBases(mirrorRoot string) remoteCachePruneOutcome {
	var outcome remoteCachePruneOutcome
	bases, unreadable, err := readMirrorBases(mirrorRoot)
	if err != nil {
		outcome.Problem = err.Error()
		return outcome
	}
	var problems []string
	if unreadable != nil {
		problems = append(problems, unreadable.Error())
	}
	for _, base := range bases {
		if _, err := os.Stat(filepath.Dir(base.database)); err == nil || !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		freed, removed, err := base.removeCounting()
		outcome.Reclaimed += freed
		if err != nil {
			problems = append(problems, fmt.Sprintf("remove git mirror base %s of deleted %s: %v", base.dir, base.database, err))
			continue
		}
		if removed {
			outcome.Removed++
		}
	}
	outcome.Problem = strings.Join(problems, "; ")
	return outcome
}
