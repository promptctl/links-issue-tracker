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
// derivation has failed to account for a configured remote that has been
// opened, so a copy that HAS drifted stops the prune instead of misdirecting it — which matters, because
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

// expectedMirror is one configured git-backed remote as the prune sees it: the
// name a refusal reports, and whether this store proves the remote has never
// been opened.
type expectedMirror struct {
	remote string
	// neverOpened is true only on evidence: this store holds no tracking branch
	// under the remote's name. Dolt sets `remotes/<remote>/<branch>` on every
	// push and fetch that lands, and writes a mirror only when a remote is
	// used, so a remote with no tracking branch has no mirror anywhere for a
	// derivation to miss. The evidence is keyed by the remote's name, never by
	// its URL, so a wrong key derivation cannot forge it. The zero value claims
	// nothing: an entry built without the evidence counts as opened and keeps
	// the refusal armed.
	neverOpened bool
}

// remoteCachePlan names the cache directories no configured remote maps to. It
// exists only when the derivation accounted for every remote that has been
// opened, so holding one is itself the proof that the keys matched reality.
// [LAW:parse-dont-validate] the plan is a type that could not have been
// constructed before the check ran; nothing downstream re-asks whether deleting
// is safe.
type remoteCachePlan struct {
	abandoned []string
	// neverOpened names the remotes with no directory that the plan excused on
	// the evidence that they have never been opened. Without it, each one would
	// have refused the plan.
	neverOpened []string
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
// the entire cache with no error raised anywhere. A broken derivation shows
// itself as a remote that has been opened and has no directory, so when such a
// remote exists AND there is something to delete, the prune declines wholesale.
// [LAW:no-silent-failure]
//
// A remote that has never been opened is excused: it has no mirror for a broken
// derivation to miss, and the evidence for that (expectedMirror.neverOpened)
// does not pass through the derivation. The refusal is NOT narrowed to the
// remote being pushed instead. That reading looks safe and is not: derivation
// drift is URL-shape-specific, which is the only kind this code has met, so
// verifying the pushed remote's key says nothing about a differently-shaped
// sibling — and under a narrowed gate that sibling's LIVE mirror is exactly
// what lands in `abandoned` and gets deleted.
//
// A store that has never pushed trips nothing: it has no directories, so there
// is nothing to delete.
func planRemoteCachePrune(expected map[string]expectedMirror, onDisk []string) (remoteCachePlan, error) {
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

	var unaccounted, neverOpened []string
	for key, mirror := range expected {
		if _, found := present[key]; found {
			continue
		}
		if mirror.neverOpened {
			neverOpened = append(neverOpened, mirror.remote)
			continue
		}
		unaccounted = append(unaccounted, fmt.Sprintf("%s→%s", mirror.remote, key))
	}
	sort.Strings(unaccounted)
	sort.Strings(neverOpened)

	if len(abandoned) > 0 && len(unaccounted) > 0 {
		return remoteCachePlan{}, fmt.Errorf(
			"declining to prune: %d cache director%s match no configured remote, but %d remote%s this "+
				"store holds tracking branches for also %s no directory (%s). Either the key derivation "+
				"disagrees with what Dolt actually wrote, or the mirror was built outside this store — a "+
				"push from a copy of this store, as the background mirror makes, records the tracking branch "+
				"here and keeps the mirror in the copy. While both readings stand an unmatched directory "+
				"cannot be told apart from a live mirror this code failed to find, so nothing was deleted. "+
				"One `lit sync push --remote <name>` or `lit sync fetch --remote <name>` through each remote "+
				"named above creates its directory here and settles it",
			len(abandoned), plural(len(abandoned), "y", "ies"),
			len(unaccounted), plural(len(unaccounted), "", "s"),
			plural(len(unaccounted), "has", "have"),
			strings.Join(unaccounted, ", "))
	}
	return remoteCachePlan{abandoned: abandoned, neverOpened: neverOpened}, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// remoteCacheCollection is what one collection job over the remote cache has to
// say for itself: how much it removed, the bytes that freed, and why it stopped
// short, if it did. It is produced on every path — success, refusal, and I/O
// failure alike — because a collection rides on a push that already succeeded
// or is about to, must never retract it, and must never go quiet either.
// [LAW:dataflow-not-control-flow] the caller runs the same steps every time and
// renders this value; it does not branch on whether a collection ran.
//
// Each job is its own named type over this shape (remoteCachePruneOutcome,
// refSweepOutcome), so the type a caller holds says which job it is reporting
// on and the rendering below is written once. [LAW:one-type-per-behavior]
type remoteCacheCollection struct {
	Removed   int
	Reclaimed int64
	// Problem is why the job did not run to completion, empty when it did. It
	// carries refusals and I/O failures alike: from the caller's seat both mean
	// "the cache was not collected this run, and here is why".
	Problem string
}

// report renders the line a caller surfaces, naming the job and what it
// removes, and is empty exactly when the job looked and found nothing to do.
//
// Empty is not a swallowed failure here: every state that matters — work
// performed, work declined, I/O failure — renders non-empty, so nothing a reader
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
// itself, including the remotes it excused as never opened.
type remoteCachePruneOutcome struct {
	remoteCacheCollection
	// NeverOpened is the plan's neverOpened: remotes with no directory that the
	// prune went ahead beside rather than refusing over.
	NeverOpened []string
}

// Report renders the prune's line; see remoteCacheCollection.report. The
// excused remotes ride on a line that already speaks, because excusing one
// matters only when the prune acted on the plan, and it must never make a
// quiet run speak.
func (o remoteCachePruneOutcome) Report() string {
	line := o.report("remote-cache prune", "abandoned mirror", "abandoned mirrors")
	if line == "" || len(o.NeverOpened) == 0 {
		return line
	}
	n := len(o.NeverOpened)
	return fmt.Sprintf("%s; excused %s %s: this store holds no tracking branch for %s, so %s never been opened",
		line, plural(n, "remote", "remotes"), strings.Join(o.NeverOpened, ", "),
		plural(n, "it", "them"), plural(n, "it has", "they have"))
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

// expectedRemoteCacheKeys derives the cache key of every git-backed remote,
// mapped to the names of the remotes that derive it — usually one, and more
// only when two remotes are spelled alike — so a refusal can say which remote
// it could not account for.
func expectedRemoteCacheKeys(remotes []storage.SyncRemote) (map[string][]string, error) {
	expected := make(map[string][]string, len(remotes))
	for _, remote := range remotes {
		key, gitBacked, err := remoteCacheKey(remote.URL)
		if err != nil {
			return nil, err
		}
		if !gitBacked {
			continue
		}
		expected[key] = append(expected[key], remote.Name)
	}
	return expected, nil
}

// expectedMirrors pairs each expected key with the evidence of whether it has
// been opened, read from trackingBranches — this store's
// `remotes/<remote>/<branch>` names. A key's mirror has never been opened only
// if none of the remotes deriving it has a tracking branch.
func expectedMirrors(keys map[string][]string, trackingBranches []string) map[string]expectedMirror {
	opened := func(remote string) bool {
		// A prefix can only err toward "opened": remote `a` also claims the
		// branches of a remote named `a/b`, which keeps the refusal armed and
		// never disarms it.
		prefix := "remotes/" + remote + "/"
		return slices.ContainsFunc(trackingBranches, func(b string) bool { return strings.HasPrefix(b, prefix) })
	}
	mirrors := make(map[string]expectedMirror, len(keys))
	for key, remotes := range keys {
		mirrors[key] = expectedMirror{
			remote:      strings.Join(remotes, " and "),
			neverOpened: !slices.ContainsFunc(remotes, opened),
		}
	}
	return mirrors
}

// trackingBranchNames lists this store's remote-tracking branches, each named
// `remotes/<remote>/<branch>`.
func (s *Store) trackingBranchNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM dolt_remote_branches`)
	if err != nil {
		return nil, fmt.Errorf("list remote-tracking branches: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan remote-tracking branch: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate remote-tracking branches: %w", err)
	}
	return names, nil
}

// readRemoteCachePlan reads the plan's three inputs as they stand now — the
// configured remotes, the tracking branches that say which have been opened,
// and the directories on disk — and plans from them.
func (s *Store) readRemoteCachePlan(ctx context.Context, base string) (remoteCachePlan, error) {
	remotes, err := s.SyncListRemotes(ctx)
	if err != nil {
		return remoteCachePlan{}, err
	}
	branches, err := s.trackingBranchNames(ctx)
	if err != nil {
		return remoteCachePlan{}, err
	}
	keys, err := expectedRemoteCacheKeys(remotes)
	if err != nil {
		return remoteCachePlan{}, err
	}
	onDisk, err := listRemoteCacheKeys(base)
	if err != nil {
		return remoteCachePlan{}, err
	}
	return planRemoteCachePrune(expectedMirrors(keys, branches), onDisk)
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
// plan is taken again before each directory instead of trusted from the start.
// The plan is a snapshot and this loop has no bound on how long it runs. A
// sibling process that re-spells a remote back to a spelling this cache still
// holds and pushes through it makes Dolt write a LIVE mirror at a key this loop
// is already carrying; a first push through a remote the plan excused as never
// opened takes away the evidence it was excused on. Deleting on the snapshot
// would be the live-mirror deletion this whole feature exists to prevent,
// reached through a clock instead of a bad derivation. [LAW:one-source-of-truth]
// the decision that deletes reads the inputs, not a copy of them.
//
// A directory removed in error costs a re-clone on the
// next open — dbfactory.ensureBareRepo re-creates a missing cache — and never a
// lost commit: the store's own data lives in `.dolt/noms`, which this never
// touches. That is why the prune reports rather than fails; it cannot damage
// anything a push has not already made durable on the remote.
func (s *Store) pruneRemoteCache(ctx context.Context) remoteCachePruneOutcome {
	base := s.remoteCacheBase()
	plan, err := s.readRemoteCachePlan(ctx, base)
	if err != nil {
		return remoteCachePruneOutcome{remoteCacheCollection: remoteCacheCollection{Problem: err.Error()}}
	}

	outcome := remoteCachePruneOutcome{NeverOpened: plan.neverOpened}
	var problems []string
	for _, key := range plan.abandoned {
		now, err := s.readRemoteCachePlan(ctx, base)
		if err != nil {
			// A plan that cannot be taken, or now refuses, closes the gate for
			// every key still to come, not only this one.
			problems = append(problems, fmt.Errorf("re-check abandoned mirror %s: %w", key, err).Error())
			break
		}
		if !slices.Contains(now.abandoned, key) {
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
	// permanently unremovable directory a head-of-line blocker: plan.abandoned
	// is sorted, so that same entry is reached first on every push forever and
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
		for _, entry := range entries {
			n, _, err := removeTreeCounting(filepath.Join(path, entry.Name()))
			freed += n
			if err != nil {
				return freed, false, err
			}
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
// [LAW:no-silent-failure] [LAW:one-source-of-truth] the path comes from the
// same key derivation the prune uses, pinned by TestRemoteCacheKeyMatchesDoltLayout.
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
	key, gitBacked, err := remoteCacheKey(remotes[index].URL)
	if err != nil || !gitBacked {
		return false, err
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
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "cat-file", "--batch-check")
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
