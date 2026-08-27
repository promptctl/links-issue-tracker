package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Dolt gives every git-backed remote its own bare-repo mirror under
// `<db>/.dolt/git-remote-cache/<sha256(url|ref)>/repo.git`, and it deletes one
// never. The key hashes the URL with `git+` stripped from its scheme and any
// query and fragment cleared — normalized, but nowhere near canonicalized, so
// any re-spelling that survives that stripping — a GitHub org rename, an
// scp-vs-ssh rewrite — sends the next open to a fresh key, which clones a whole
// new mirror and abandons the previous one forever.
// Measured on this repository when this file landed: 142 MB of cache, of which
// 97 MB was live and 45 MB was three abandoned mirrors of two long-gone remote
// spellings. Nothing upstream collects them; Dolt ships no prune, evict, or
// cleanup for this directory.
//
// [LAW:effects-at-boundaries] Which directories are abandoned is a pure function
// of two key sets (planRemoteCachePrune); only pruneRemoteCache touches disk.

const (
	// remoteCacheDirName is dbfactory's cache directory, relative to the
	// database directory that holds `.dolt`.
	remoteCacheDirName = "git-remote-cache"

	// defaultGitRemoteRef mirrors dbfactory's defaultGitRef. lit never supplies
	// GitRefParam, so every cache key reachable from this store derives with it.
	// A future ref parameter would change every key, which is exactly what
	// TestRemoteCacheKeyMatchesDoltLayout exists to catch.
	defaultGitRemoteRef = "refs/dolt/data"

	// gitBackedURLSchemePrefix marks the Dolt remote URLs that get a mirror.
	// A remote spelled any other way has no cache directory at all.
	gitBackedURLSchemePrefix = "git+"
)

// remoteCacheKey reproduces the directory name dbfactory derives for a Dolt git
// remote URL: sha256 over the underlying (non-`git+`) URL, a literal "|", and
// the ref.
//
// The three outcomes are distinct on purpose. A git-backed URL yields a key. A
// remote spelled without the `git+` scheme is not an error — it simply has no
// mirror, so it contributes no key and the prune carries on. Only a URL that
// will not parse is a failure. Collapsing the middle case into either of the
// others would be an enumeration gap: treat it as an error and one ordinary
// non-git remote disables the prune forever; treat it as a key and the prune
// invents a directory that was never written.
//
// [LAW:one-source-of-truth] exception: this duplicates dbfactory.cacheRepoPath,
// which is unexported. The duplication is deliberate and bounded on two sides.
// TestRemoteCacheKeyMatchesDoltLayout pins it against a cache directory Dolt
// itself created, so a scheme change upstream fails a test rather than producing
// quiet nonsense. And planRemoteCachePrune declines to delete anything once the
// derivation has failed to account for a configured remote, so a copy that HAS
// drifted stops the prune instead of misdirecting it — which matters, because
// the drift this guards against is not hypothetical: a home-relative scp URL
// normalizes to `ssh://git@host/./path`, and dropping that `/./` yields a
// perfectly plausible key that points at no directory on disk.
func remoteCacheKey(remoteURL string) (key string, gitBacked bool, err error) {
	parsed, err := url.Parse(strings.TrimSpace(remoteURL))
	if err != nil {
		return "", false, fmt.Errorf("parse dolt remote url %q: %w", remoteURL, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if !strings.HasPrefix(scheme, gitBackedURLSchemePrefix) {
		return "", false, nil
	}
	// dbfactory hashes the string parseGitRemoteFactoryURL produces: the same
	// URL with `git+` stripped from the scheme and query and fragment cleared.
	underlying := *parsed
	underlying.Scheme = strings.TrimPrefix(scheme, gitBackedURLSchemePrefix)
	underlying.RawQuery = ""
	underlying.Fragment = ""
	sum := sha256.Sum256([]byte(underlying.String() + "|" + defaultGitRemoteRef))
	return hex.EncodeToString(sum[:]), true, nil
}

// isRemoteCacheKey reports whether a directory name is one of dbfactory's cache
// keys — a lowercase hex sha256 and nothing else. Anything else under the cache
// base was not put there by dbfactory, so this prune does not own it and must
// never delete it. [LAW:parse-dont-validate] the name's shape is the only
// evidence that a directory belongs to the cache; a name failing this is not a
// cache entry at all, rather than a cache entry we are unsure about.
func isRemoteCacheKey(name string) bool {
	if len(name) != sha256.Size*2 {
		return false
	}
	if name != strings.ToLower(name) {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

// remoteCachePlan names the cache directories no configured remote maps to. It
// exists only when the derivation accounted for every configured remote, so
// holding one is itself the proof that the keys matched reality.
// [LAW:parse-dont-validate] the plan is a type that could not have been
// constructed before the check ran; nothing downstream re-asks whether deleting
// is safe.
type remoteCachePlan struct {
	abandoned []string
}

// planRemoteCachePrune decides which cache keys are abandoned.
//
// One rule: a directory is abandoned when no configured remote derives its key.
//
// One refusal, and it is the reason this is a named function rather than a set
// subtraction at the call site. "This key is not in the expected set" means two
// incompatible things — the mirror is genuinely abandoned, or the derivation is
// wrong and never produced the live key. Left collapsed, a broken derivation
// reads as evidence about the disk: the prune deletes the live mirror, the next
// push silently re-clones it, and the feature inverts into a per-push churn of
// the entire cache with no error raised anywhere. So when some configured remote
// has no directory AND there is something to delete, the two cases cannot be
// told apart and the prune declines wholesale. [LAW:no-silent-failure]
//
// A missing directory is itself two facts wearing one shape — a broken
// derivation, or a remote nothing has opened yet, since Dolt writes a mirror on
// first use and not when a remote is configured. The refusal deliberately does
// NOT try to separate them by narrowing to the remote being pushed. That reading
// looks safe and is not: derivation drift is URL-shape-specific, which is the
// only kind this code has met, so verifying the pushed remote's key says nothing
// about a differently-shaped sibling — and under a narrowed gate that sibling's
// LIVE mirror is exactly what lands in `abandoned` and gets deleted. Refusing on
// either cause costs disk that nobody loses; separating them costs the mirror.
//
// A store that has never pushed trips nothing: it has no directories, so there
// is nothing to delete and the unaccounted keys are just a cache not yet built.
func planRemoteCachePrune(expected map[string]string, onDisk []string) (remoteCachePlan, error) {
	present := make(map[string]struct{}, len(onDisk))
	for _, key := range onDisk {
		present[key] = struct{}{}
	}

	abandoned := make([]string, 0, len(onDisk))
	for _, key := range onDisk {
		if _, wanted := expected[key]; !wanted {
			abandoned = append(abandoned, key)
		}
	}
	sort.Strings(abandoned)

	unaccounted := make([]string, 0, len(expected))
	for key, remoteName := range expected {
		if _, found := present[key]; !found {
			unaccounted = append(unaccounted, fmt.Sprintf("%s→%s", remoteName, key))
		}
	}
	sort.Strings(unaccounted)

	if len(abandoned) > 0 && len(unaccounted) > 0 {
		return remoteCachePlan{}, fmt.Errorf(
			"declining to prune: %d cache director%s match no configured remote, but %d configured "+
				"remote%s also %s no directory (%s). That is two possible facts wearing one shape, and "+
				"this code cannot tell which it is looking at: either the key derivation disagrees with "+
				"what Dolt actually wrote, or those remotes have simply never been opened — Dolt writes "+
				"a mirror on first use, never when a remote is configured. While both readings stand an "+
				"unmatched directory cannot be told apart from a live mirror this code failed to find, "+
				"so nothing was deleted. One `lit sync push --remote <name>` or `lit sync fetch --remote "+
				"<name>` through each remote named above creates its directory and settles it",
			len(abandoned), plural(len(abandoned), "y", "ies"),
			len(unaccounted), plural(len(unaccounted), "", "s"),
			plural(len(unaccounted), "has", "have"),
			strings.Join(unaccounted, ", "))
	}
	return remoteCachePlan{abandoned: abandoned}, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// remoteCachePruneOutcome is what one prune attempt has to say for itself. It is
// produced on every path — success, refusal, and I/O failure alike — because a
// prune must never retract a push that already succeeded, and must never go
// quiet either. [LAW:dataflow-not-control-flow] the caller runs the same steps
// every time and renders this value; it does not branch on whether a prune ran.
type remoteCachePruneOutcome struct {
	Removed   int
	Reclaimed int64
	// Problem is why the prune did not run to completion, empty when it did. It
	// carries refusals and I/O failures alike: from the caller's seat both mean
	// "the cache was not collected this run, and here is why".
	Problem string
}

// Report renders the line a caller surfaces, and is empty exactly when the prune
// looked and found nothing to do.
//
// Empty is not a swallowed failure here: every state that matters — work
// performed, work declined, I/O failure — renders non-empty, so nothing a reader
// would act on can reach the empty value. The routine case earns silence because
// this line rides on every push, and "nothing abandoned" repeated forever is how
// a channel stops being read before the one message that mattered arrives.
//
// A problem does not erase confirmed work. Removing a directory is not undone by
// a later error, so a run that collected two mirrors and then failed on the third
// reports both facts in that order. [LAW:no-silent-failure] the earlier arm must
// not swallow what the loop already proved.
func (o remoteCachePruneOutcome) Report() string {
	switch {
	case o.Problem != "" && o.Removed > 0:
		return fmt.Sprintf("remote-cache prune: removed %d abandoned mirror%s (%s), then failed: %s",
			o.Removed, plural(o.Removed, "", "s"), humanBytes(o.Reclaimed), o.Problem)
	case o.Problem != "":
		return "remote-cache prune: " + o.Problem
	case o.Removed > 0:
		return fmt.Sprintf("remote-cache prune: removed %d abandoned mirror%s, reclaimed %s",
			o.Removed, plural(o.Removed, "", "s"), humanBytes(o.Reclaimed))
	default:
		return ""
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// remoteCacheBase is where dbfactory keeps this store's mirrors.
//
// The path segments are not guesswork, and each has exactly one owner. Dolt's
// sqle provider roots a git cache at the *database's* own directory
// (`dbLocations[db].Abs(".")`), which the embedded connector lays down as
// `<doltRootDir>/<database>` — so the middle segment is doltDatabaseName, never
// the workspace id, which names a different thing and only coincides with the
// database name in a repository called "links".
// [LAW:one-source-of-truth] every segment comes from the constant that defines
// it: the root from the Store, the database name from doltDatabaseName, and
// `.dolt` from dbfactory rather than a second literal here.
func (s *Store) remoteCacheBase() string {
	return filepath.Join(s.doltRootDir, doltDatabaseName, dbfactory.DoltDir, remoteCacheDirName)
}

// listRemoteCacheKeys returns the cache keys present under base. A base that
// does not exist is a store that has never opened a git remote — zero keys is
// the true answer to "which mirrors are on disk", not a failure swallowed.
func listRemoteCacheKeys(base string) ([]string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read git remote cache %s: %w", base, err)
	}
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && isRemoteCacheKey(entry.Name()) {
			keys = append(keys, entry.Name())
		}
	}
	return keys, nil
}

// dirSize totals the bytes under root, so an outcome reports what pruning
// actually reclaimed rather than how many directories it touched.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// expectedRemoteCacheKeys derives the cache key of every git-backed remote,
// mapped to the remote's name so a refusal can say which remote it could not
// account for.
func expectedRemoteCacheKeys(remotes []storage.SyncRemote) (map[string]string, error) {
	expected := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		key, gitBacked, err := remoteCacheKey(remote.URL)
		if err != nil {
			return nil, err
		}
		if !gitBacked {
			continue
		}
		expected[key] = remote.Name
	}
	return expected, nil
}

// pruneRemoteCache collects the mirrors no configured remote maps to.
//
// It runs without the commit lock and needs no lock at all: a directory is
// eligible only when NO configured remote derives its key, and every open
// reaches a cache through a configured remote's key, so an abandoned mirror is
// unreachable by construction rather than by exclusion.
// [LAW:no-ambient-temporal-coupling] the eligibility rule is what owns this
// safety, not a lock some caller has to remember to still be holding.
//
// A directory removed in error costs a re-clone on the
// next open — dbfactory.ensureBareRepo re-creates a missing cache — and never a
// lost commit: the store's own data lives in `.dolt/noms`, which this never
// touches. That is why the prune reports rather than fails; it cannot damage
// anything a push has not already made durable on the remote.
func (s *Store) pruneRemoteCache(ctx context.Context) remoteCachePruneOutcome {
	remotes, err := s.SyncListRemotes(ctx)
	if err != nil {
		return remoteCachePruneOutcome{Problem: err.Error()}
	}
	expected, err := expectedRemoteCacheKeys(remotes)
	if err != nil {
		return remoteCachePruneOutcome{Problem: err.Error()}
	}
	base := s.remoteCacheBase()
	onDisk, err := listRemoteCacheKeys(base)
	if err != nil {
		return remoteCachePruneOutcome{Problem: err.Error()}
	}
	plan, err := planRemoteCachePrune(expected, onDisk)
	if err != nil {
		return remoteCachePruneOutcome{Problem: err.Error()}
	}

	outcome := remoteCachePruneOutcome{}
	var problems []string
	for _, key := range plan.abandoned {
		reclaimed, collected, err := collectAbandonedMirror(base, key)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !collected {
			continue
		}
		outcome.Removed++
		outcome.Reclaimed += reclaimed
	}
	// Every entry is attempted and a failure records itself rather than ending
	// the walk. Returning on the first error made one permanently unremovable
	// directory a head-of-line blocker: plan.abandoned is sorted, so that same
	// entry is reached first on every push forever and nothing sorting after it
	// is ever attempted again, however removable it is.
	// [LAW:dataflow-not-control-flow] which entries get visited no longer
	// depends on what the earlier ones did.
	outcome.Problem = strings.Join(problems, "; ")
	return outcome
}

// collectAbandonedMirror measures and removes one abandoned mirror, and has
// three things it can report: collected, with the bytes it reclaimed; already
// gone; or a failure naming the key.
//
// Already gone is the one worth explaining, and it is not an error. Two explicit
// pushes are not serialized against each other — the single-flight lock belongs
// to the background mirror and is a non-blocking probe, not mutual exclusion —
// so a sibling prune can take this directory between the plan and this call.
// That is the state this code was trying to reach, so reporting it would be
// inventing a failure out of a success. [LAW:no-silent-failure] cuts both ways
// here: the refusal this outcome carries only works if it is believed, and a
// prune that cries wolf whenever two pushes overlap spends exactly the
// credibility the refusal depends on.
func collectAbandonedMirror(base, key string) (reclaimed int64, collected bool, err error) {
	dir := filepath.Join(base, key)
	// Size first: once the directory is gone there is nothing left to measure,
	// and a reclaim figure nobody can check is worth nothing.
	size, err := dirSize(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("measure abandoned mirror %s: %w", key, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return 0, false, fmt.Errorf("remove abandoned mirror %s: %w", key, err)
	}
	return size, true, nil
}
