package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// Nothing upstream collects them; Dolt ships no prune, evict, or cleanup for
// this directory.
//
// [LAW:effects-at-boundaries] Which directories are abandoned is a pure function
// of two key sets (splitMirrors); only pruneRemoteCache touches disk.

// remoteCacheDirName is dbfactory's cache directory, relative to the database
// directory that holds `.dolt`.
const remoteCacheDirName = "git-remote-cache"

// mirrorGit builds "git <args...>" against the mirror at gitDir. It is the one
// way lit runs git on a mirror. [LAW:single-enforcer] core.longpaths lets git for
// Windows open paths past MAX_PATH (260 characters): a mirror ref sits about 175
// characters below the database directory, so a database a few directories deep
// passes it. git hands the setting on to the git processes it spawns, and other
// platforms ignore the key. Dolt's own git commands on a mirror pass it too
// (Patch 12 in FORKS.md).
func mirrorGit(ctx context.Context, gitDir string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "git", append([]string{"-c", gitLongPaths, "--git-dir", gitDir}, args...)...)
}

// gitLongPaths is the setting mirrorGit passes to git.
const gitLongPaths = "core.longpaths=true"

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

// splitMirrors divides the cache directories on disk into the live ones, which
// a configured remote's key names, and the abandoned rest, each sorted. Every
// open reaches a mirror through a configured remote's key, so an abandoned
// directory is one no open can reach, and deleting it costs nothing a later
// open would use. A configured remote with no directory has not been opened
// here yet; its key is simply absent from onDisk and changes nothing.
func splitMirrors(keys map[string]string, onDisk []string) (live, abandoned []string) {
	configured := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		configured[key] = struct{}{}
	}
	for _, key := range onDisk {
		if _, ok := configured[key]; ok {
			live = append(live, key)
		} else {
			abandoned = append(abandoned, key)
		}
	}
	sort.Strings(live)
	sort.Strings(abandoned)
	return live, abandoned
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// remoteCacheCollection is what one collection job over the remote cache has to
// say for itself: how much it removed, the bytes that freed, and why it stopped
// short, if it did. It is produced on every path — success and failure alike —
// because a collection rides on a push that already succeeded or is about to,
// must never retract it, and must never go quiet either.
// [LAW:dataflow-not-control-flow] the caller runs the same steps every time and
// renders this value; it does not branch on whether a collection ran.
//
// Each job is its own named type over this shape (remoteCachePruneOutcome,
// refSweepOutcome), so the type a caller holds says which job it is reporting
// on and the rendering below is written once. [LAW:one-type-per-behavior]
type remoteCacheCollection struct {
	Removed   int
	Reclaimed int64
	// Problem is why the job did not run to completion, empty when it did:
	// "the cache was not collected this run, and here is why".
	Problem string
}

// report renders the line a caller surfaces, naming the job and what it
// removes, and is empty exactly when the job looked and found nothing to do.
//
// Empty is not a swallowed failure here: every state that matters — work
// performed, I/O failure — renders non-empty, so nothing a reader
// would act on can reach the empty value. The routine case earns silence because
// this line rides on every push, and "nothing abandoned" repeated forever is how
// a channel stops being read before the one message that mattered arrives.
//
// A problem does not erase confirmed work. A removal is not undone by a later
// error, so a run that collected two things and then failed on the third
// reports both facts in that order. [LAW:no-silent-failure] the earlier arm must
// not swallow what the loop already proved.
func (o remoteCacheCollection) report(job, one, many string) string {
	// Bytes freed with nothing removed is a sibling prune that finished a
	// directory this one was part-way through: confirmed work all the same.
	didWork := o.Removed > 0 || o.Reclaimed > 0
	switch {
	case o.Problem != "" && didWork:
		return fmt.Sprintf("%s: removed %d %s (%s), then failed: %s",
			job, o.Removed, plural(o.Removed, one, many), humanBytes(o.Reclaimed), o.Problem)
	case o.Problem != "":
		return job + ": " + o.Problem
	case didWork:
		return fmt.Sprintf("%s: removed %d %s, reclaimed %s",
			job, o.Removed, plural(o.Removed, one, many), humanBytes(o.Reclaimed))
	default:
		return ""
	}
}

// remoteCachePruneOutcome is what one prune of abandoned mirrors has to say for
// itself.
type remoteCachePruneOutcome remoteCacheCollection

// Report renders the prune's line; see remoteCacheCollection.report.
func (o remoteCachePruneOutcome) Report() string {
	return remoteCacheCollection(o).report("remote-cache prune", "abandoned mirror", "abandoned mirrors")
}

// humanBytes renders a byte count for operator-facing output. Package-private,
// and shared by both of this package's maintenance reporters — compaction's
// footprintDelta and the remote-cache collections' report — so the two cannot
// disagree about how a size is spelled. The CLI renders no size of its own; it
// prints the accounts these reporters already produced, which is why one
// spelling here is enough to make every spelling agree.
// [LAW:one-source-of-truth]
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

// gitCacheRoot is the directory dbfactory roots this store's git caches at:
// its GitCacheRootParam, under which it keeps each mirror in
// remoteCacheBase.
//
// The path segments are not guesswork, and each has exactly one owner. Dolt's
// sqle provider roots a git cache at the *database's* own directory
// (`dbLocations[db].Abs(".")`), which the embedded connector lays down as
// `<doltRootDir>/<database>` — so the last segment is doltDatabaseName, never
// the workspace id, which names a different thing and only coincides with the
// database name in a repository called "links".
// [LAW:one-source-of-truth] every segment comes from the constant that defines
// it: the root from the Store, the database name from doltDatabaseName, and
// `.dolt` from dbfactory rather than a second literal here.
func (s *Store) gitCacheRoot() string {
	return filepath.Join(s.doltRootDir, doltDatabaseName)
}

// remoteCacheBase is where dbfactory keeps this store's mirrors.
func (s *Store) remoteCacheBase() string {
	return filepath.Join(s.gitCacheRoot(), dbfactory.DoltDir, remoteCacheDirName)
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

// dirSize totals the bytes under root, so an outcome reports what collecting
// actually reclaimed rather than how many things it touched.
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

// remoteMirrorKeys names, by remote, the cache directory of every git-backed
// remote configured on this store. A remote that is not git-backed has no
// mirror and no entry.
//
// [LAW:one-source-of-truth] each name is dbfactory.GitRemoteCacheKey's, fed the
// url and params dolt_remotes holds for the remote: the parse, the factory
// choice and the hash CreateDB writes the directory with (FORKS.md, dolt patch
// 8). So the key is the directory Dolt writes for every URL shape, not a copy of
// its derivation that a shape it never met could make drift.
func (s *Store) remoteMirrorKeys(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, url, params FROM dolt_remotes`)
	if err != nil {
		return nil, fmt.Errorf("list dolt remotes: %w", err)
	}
	defer rows.Close()
	keys := map[string]string{}
	for rows.Next() {
		var name, remoteURL string
		var rawParams sql.NullString
		if err := rows.Scan(&name, &remoteURL, &rawParams); err != nil {
			return nil, fmt.Errorf("scan dolt remote: %w", err)
		}
		params, err := decodeRemoteParams(name, rawParams)
		if err != nil {
			return nil, err
		}
		key, gitBacked, err := dbfactory.GitRemoteCacheKey(remoteURL, params)
		if err != nil {
			return nil, fmt.Errorf("derive cache key of remote %q (%s): %w", name, remoteURL, err)
		}
		if gitBacked {
			keys[name] = key
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dolt remotes: %w", err)
	}
	return keys, nil
}

// decodeRemoteParams is the params column dolt_remotes holds for the remote
// name, as the map Dolt opens the remote with. A remote with none has an
// empty map: the column is SQL NULL, or JSON null where Dolt rendered a nil
// map (dtables remotes_table.go), which decoding sets the map to.
func decodeRemoteParams(name string, raw sql.NullString) (map[string]interface{}, error) {
	var decoded map[string]interface{}
	if raw.Valid {
		if err := json.Unmarshal([]byte(raw.String), &decoded); err != nil {
			return nil, fmt.Errorf("decode params of remote %q: %w", name, err)
		}
	}
	params := map[string]interface{}{}
	maps.Copy(params, decoded)
	return params, nil
}

// readAbandonedMirrors reads the configured remotes' keys and the directories
// under base as they stand now, and names the abandoned ones.
func (s *Store) readAbandonedMirrors(ctx context.Context, base string) ([]string, error) {
	keys, err := s.remoteMirrorKeys(ctx)
	if err != nil {
		return nil, err
	}
	onDisk, err := listRemoteCacheKeys(base)
	if err != nil {
		return nil, err
	}
	_, abandoned := splitMirrors(keys, onDisk)
	return abandoned, nil
}

// pruneRemoteCache collects the mirrors no configured remote maps to.
//
// It runs without the commit lock, and what makes a removal safe is the
// eligibility rule rather than exclusion: a directory is eligible only when NO
// configured remote derives its key, and every open reaches a cache through a
// configured remote's key, so a mirror stays unreachable for exactly as long as
// it stays abandoned. [LAW:no-ambient-temporal-coupling] the rule owns this
// safety, not a lock some caller has to remember to still be holding.
//
// "For as long as it stays abandoned" is the entire caveat, and it is why the
// abandoned set is read again before each directory instead of trusted from the
// start. The first read is a snapshot and this loop has no bound on how long it
// runs. A sibling process that re-spells a remote back to a spelling this cache
// still holds and pushes through it makes Dolt write a LIVE mirror at a key this
// loop is already carrying. Deleting on the snapshot would be the live-mirror
// deletion this whole feature exists to prevent. [LAW:one-source-of-truth] the
// decision that deletes reads the inputs, not a copy of them.
//
// A directory removed in error costs a re-clone on the
// next open — dbfactory.ensureBareRepo re-creates a missing cache — and never a
// lost commit: the store's own data lives in `.dolt/noms`, which this never
// touches. That is why the prune reports rather than fails; it cannot damage
// anything a push has not already made durable on the remote.
func (s *Store) pruneRemoteCache(ctx context.Context) remoteCachePruneOutcome {
	base := s.remoteCacheBase()
	read := func() ([]string, error) { return s.readAbandonedMirrors(ctx, base) }
	abandoned, err := read()
	if err != nil {
		return remoteCachePruneOutcome{Problem: err.Error()}
	}
	return collectAbandonedMirrors(base, abandoned, read)
}

// collectAbandonedMirrors removes the abandoned mirrors under base, reading the
// abandoned set again through reread before each one and removing a key only
// if the fresh read still names it.
func collectAbandonedMirrors(base string, abandoned []string, reread func() ([]string, error)) remoteCachePruneOutcome {
	var outcome remoteCachePruneOutcome
	var problems []string
	for _, key := range abandoned {
		now, err := reread()
		if err != nil {
			// A read that fails closes the gate for every key still to come,
			// not only this one.
			problems = append(problems, fmt.Errorf("re-check abandoned mirror %s: %w", key, err).Error())
			break
		}
		if !slices.Contains(now, key) {
			continue
		}
		reclaimed, collected, err := collectAbandonedMirror(base, key)
		outcome.Reclaimed += reclaimed
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if collected {
			outcome.Removed++
		}
	}
	// Every entry is attempted and a failure to remove one records itself
	// rather than ending the walk. Returning on the first error would make one
	// permanently unremovable directory a head-of-line blocker: abandoned is
	// sorted, so that same entry is reached first on every push forever and
	// nothing sorting after it is ever attempted again, however removable it
	// is. [LAW:dataflow-not-control-flow] which entries get visited does not
	// depend on what the earlier ones did.
	outcome.Problem = strings.Join(problems, "; ")
	return outcome
}

// collectAbandonedMirror removes one abandoned mirror and reports what this
// call itself did: the bytes of every file it unlinked, and whether it
// unlinked the mirror's directory. Its bytes stand even when it fails part-way,
// because a later failure does not restore them.
//
// Ownership is per unlink. Two explicit pushes are not serialized against each
// other — the single-flight lock belongs to the background mirror and is a
// non-blocking probe, not mutual exclusion — so a sibling prune can be
// collecting the same directory at the same moment. A file is unlinked once,
// by one of them, so each byte is counted by exactly one prune and the figures
// of two overlapping prunes add up to what they removed between them. A path
// already gone is a sibling's work: neither an error nor a reclaim of this
// call's, since reporting it as either would invent a failure or a figure out
// of someone else's success. [LAW:no-silent-failure]
//
// A crash part-way leaves the directory still named by its key, so the next
// prune classifies it and finishes the job. Nothing is ever staged under a name
// isRemoteCacheKey refuses, which is what would strand it.
func collectAbandonedMirror(base, key string) (reclaimed int64, collected bool, err error) {
	reclaimed, collected, err = removeTreeCounting(filepath.Join(base, key))
	if err != nil {
		return reclaimed, false, fmt.Errorf("remove abandoned mirror %s: %w", key, err)
	}
	return reclaimed, collected, nil
}

// removeTreeCounting removes path and everything under it, depth first,
// returning the bytes of the files this call unlinked and whether it unlinked
// path itself. A path already gone was removed by someone else and counts for
// nothing here.
func removeTreeCounting(path string) (freed int64, removed bool, err error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		// A child that will not go does not stop its siblings, as with
		// os.RemoveAll: one stuck file must not pin the rest of a mirror's
		// bytes on disk on every prune that reaches it.
		var failed []error
		for _, entry := range entries {
			n, _, err := removeTreeCounting(filepath.Join(path, entry.Name()))
			freed += n
			if err != nil {
				failed = append(failed, err)
			}
		}
		if len(failed) > 0 {
			return freed, false, errors.Join(failed...)
		}
	}
	if err := os.Remove(path); errors.Is(err, fs.ErrNotExist) {
		return freed, false, nil
	} else if err != nil {
		return freed, false, err
	}
	if !info.IsDir() {
		freed += info.Size()
	}
	return freed, true, nil
}

// SyncRemoteMirrorHolds reports whether this store's git mirror of remote —
// the bare repo dbfactory keeps at `<remoteCacheBase>/<key>/repo.git` — holds
// every one of commits as a git commit object. See the storage.Syncer
// contract for what the answer proves. It contacts no network: the mirror is
// a local directory, asked with one `git cat-file --batch-check`.
//
// "Cannot prove it" and "could not ask" are kept apart. A remote whose URL has
// no mirror (not git-backed), a mirror not yet on disk, an empty commit list,
// or a commit the mirror lacks are all false with no error — each is a true
// "this mirror does not hold them". A remote this store does not know, a URL
// that will not parse, or a git that fails to answer is the error.
// [LAW:no-silent-failure] [LAW:one-source-of-truth] the path comes from
// remoteMirrorKeys, the key Dolt itself writes the mirror at.
func (s *Store) SyncRemoteMirrorHolds(ctx context.Context, remote string, commits []string) (bool, error) {
	if len(commits) == 0 {
		return false, nil
	}
	remotes, err := s.SyncListRemotes(ctx)
	if err != nil {
		return false, err
	}
	index := slices.IndexFunc(remotes, func(r storage.SyncRemote) bool { return r.Name == remote })
	if index < 0 {
		return false, fmt.Errorf("remote %q is not configured on this store", remote)
	}
	keys, err := s.remoteMirrorKeys(ctx)
	if err != nil {
		return false, err
	}
	key, gitBacked := keys[remote]
	if !gitBacked {
		return false, nil
	}
	gitDir := filepath.Join(s.remoteCacheBase(), key, "repo.git")
	if _, err := os.Stat(gitDir); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("stat git mirror of remote %q: %w", remote, err)
	}
	return gitDirHoldsCommits(ctx, gitDir, commits)
}

// gitDirHoldsCommits asks one git repository whether every one of commits is
// a commit object in it. `cat-file --batch-check` answers each input line with
// "<oid> <type> <size>" or "<input> missing", so absence is an answer and only
// git failing to run is an error.
func gitDirHoldsCommits(ctx context.Context, gitDir string, commits []string) (bool, error) {
	cmd := mirrorGit(ctx, gitDir, "cat-file", "--batch-check")
	cmd.Stdin = strings.NewReader(strings.Join(commits, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("ask git mirror %s for %d commit(s): %w", gitDir, len(commits), err)
	}
	answers := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(answers) != len(commits) {
		return false, fmt.Errorf("ask git mirror %s: %d answer(s) for %d commit(s): %q", gitDir, len(answers), len(commits), out)
	}
	for i, answer := range answers {
		if !strings.HasPrefix(answer, commits[i]+" commit ") {
			return false, nil
		}
	}
	return true, nil
}
