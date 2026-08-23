package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// litRepoWithCommit creates a git repository that `git worktree add` will accept
// (worktrees need a commit to branch from).
func litRepoWithCommit(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	run(t, repo, "git", "init")
	run(t, repo, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	return repo
}

// TestAcceptanceThreeActorsTwoTokens is the ticket's acceptance scenario run
// literally: two worktrees of one repository, two sessions in one of them —
// three actors, exactly two distinct tokens, stable across invocations.
//
// The "sessions" are separate calls rather than separate processes on purpose:
// a session boundary is exactly what a fresh call reproduces, since nothing is
// carried in memory between them and the token is read from disk every time.
// That is the property the design exists to produce — a new session inherits its
// checkout's identity with no re-briefing.
func TestAcceptanceThreeActorsTwoTokens(t *testing.T) {
	primary := litRepoWithCommit(t)
	linked := filepath.Join(t.TempDir(), "linked")
	run(t, primary, "git", "worktree", "add", linked)

	primaryInfo, err := Resolve(primary)
	if err != nil {
		t.Fatalf("Resolve(primary) error = %v", err)
	}
	linkedInfo, err := Resolve(linked)
	if err != nil {
		t.Fatalf("Resolve(linked) error = %v", err)
	}

	// Session one and session two, both in the primary checkout.
	sessionOne, err := EnsureStream(primaryInfo.PrivateGitDir)
	if err != nil {
		t.Fatalf("EnsureStream(session one) error = %v", err)
	}
	sessionTwo, err := EnsureStream(primaryInfo.PrivateGitDir)
	if err != nil {
		t.Fatalf("EnsureStream(session two) error = %v", err)
	}
	// Session three, in the linked worktree.
	sessionThree, err := EnsureStream(linkedInfo.PrivateGitDir)
	if err != nil {
		t.Fatalf("EnsureStream(session three) error = %v", err)
	}

	if sessionOne.Value() != sessionTwo.Value() {
		t.Fatalf("two sessions in one checkout are one claimant and must share its token: %q vs %q",
			sessionOne.Value(), sessionTwo.Value())
	}
	if sessionOne.Value() == sessionThree.Value() {
		t.Fatalf("two worktrees must mint distinct tokens; both got %q", sessionOne.Value())
	}
	for _, id := range []StreamID{sessionOne, sessionTwo, sessionThree} {
		if !id.Present() {
			t.Fatal("EnsureStream must always yield a present token")
		}
	}
}

// TestOneRepositoryOneStoreManyIdentities states the mechanism the whole ticket
// rests on: the two worktrees agree about WHERE THE BACKLOG IS and disagree
// about WHO THEY ARE. Those come from two different git questions
// (--git-common-dir vs --git-dir), and if either ever answered like the other,
// claims would be impossible (one identity for all worktrees) or the backlog
// would fragment (one store per worktree).
func TestOneRepositoryOneStoreManyIdentities(t *testing.T) {
	primary := litRepoWithCommit(t)
	linked := filepath.Join(t.TempDir(), "linked")
	run(t, primary, "git", "worktree", "add", linked)

	primaryInfo, err := Resolve(primary)
	if err != nil {
		t.Fatalf("Resolve(primary) error = %v", err)
	}
	linkedInfo, err := Resolve(linked)
	if err != nil {
		t.Fatalf("Resolve(linked) error = %v", err)
	}

	if primaryInfo.StorageDir != linkedInfo.StorageDir {
		t.Fatalf("worktrees of one repository must share one store: %q vs %q",
			primaryInfo.StorageDir, linkedInfo.StorageDir)
	}
	if primaryInfo.WorkspaceID != linkedInfo.WorkspaceID {
		t.Fatalf("one store means one workspace id: %q vs %q",
			primaryInfo.WorkspaceID, linkedInfo.WorkspaceID)
	}
	if primaryInfo.PrivateGitDir == "" || linkedInfo.PrivateGitDir == "" {
		t.Fatal("Resolve must populate PrivateGitDir")
	}
	// Canonicalized before comparing, because the question is whether these are
	// the same DIRECTORY, not whether they are the same string. Git answers the
	// primary with a relative ".git" and the linked worktree with an absolute
	// path, and on macOS a temp dir is reached through the /var -> /private/var
	// symlink — so two spellings of one directory compare unequal and a raw
	// string comparison would pass even if both worktrees shared one identity.
	primaryGitDir, err := filepath.EvalSymlinks(primaryInfo.PrivateGitDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(primary) error = %v", err)
	}
	linkedGitDir, err := filepath.EvalSymlinks(linkedInfo.PrivateGitDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(linked) error = %v", err)
	}
	if primaryGitDir == linkedGitDir {
		t.Fatalf("worktrees must have distinct private git dirs; both got %q", primaryGitDir)
	}
}

// TestPrivateGitDirResolvesFromSubdirectory guards the anchoring rule: git prints
// these paths relative to the invocation cwd, so a resolution from a nested
// directory must still land on the checkout's own git dir rather than on a path
// interpreted against the wrong anchor.
func TestPrivateGitDirResolvesFromSubdirectory(t *testing.T) {
	repo := litRepoWithCommit(t)
	nested := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll error = %v", err)
	}

	fromRoot, err := Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve(repo) error = %v", err)
	}
	fromNested, err := Resolve(nested)
	if err != nil {
		t.Fatalf("Resolve(nested) error = %v", err)
	}

	rootDir, err := filepath.EvalSymlinks(fromRoot.PrivateGitDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(fromRoot) error = %v", err)
	}
	nestedDir, err := filepath.EvalSymlinks(fromNested.PrivateGitDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(fromNested) error = %v", err)
	}
	if rootDir != nestedDir {
		t.Fatalf("one checkout has one identity wherever it is invoked from: %q vs %q", rootDir, nestedDir)
	}
	// A checkout invoked from a subdirectory must be the same claimant.
	deep, err := EnsureStream(fromNested.PrivateGitDir)
	if err != nil {
		t.Fatalf("EnsureStream(nested) error = %v", err)
	}
	shallow, err := ReadStream(fromRoot.PrivateGitDir)
	if err != nil {
		t.Fatalf("ReadStream(root) error = %v", err)
	}
	if deep.Value() != shallow.Value() {
		t.Fatalf("subdirectory invocation minted a second identity: %q vs %q", deep.Value(), shallow.Value())
	}
}

// TestReadOnlyNeverMints is the "absent until first mutation" half of the
// acceptance scenario. A checkout that has only ever been read must carry no
// identity AND leave no file behind — the second assertion matters because a
// token file created with empty contents would satisfy an in-memory check while
// still having written to a checkout that never mutated.
func TestReadOnlyNeverMints(t *testing.T) {
	repo := litRepoWithCommit(t)
	info, err := Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	for range 3 {
		id, err := ReadStream(info.PrivateGitDir)
		if err != nil {
			t.Fatalf("ReadStream() error = %v", err)
		}
		if id.Present() {
			t.Fatalf("a never-mutated checkout must have no identity; got %q", id.Value())
		}
	}
	if _, err := os.Stat(filepath.Join(info.PrivateGitDir, streamTokenFile)); !os.IsNotExist(err) {
		t.Fatalf("reading must not create the token file; Stat error = %v", err)
	}
}

// TestIdentityDiesWithTheWorktree is the "gone with the deleted worktree" half.
// Nothing of lit's cleans this up: the token is inside the directory git itself
// removes, which is the entire reason for putting it there.
func TestIdentityDiesWithTheWorktree(t *testing.T) {
	primary := litRepoWithCommit(t)
	linked := filepath.Join(t.TempDir(), "linked")
	run(t, primary, "git", "worktree", "add", linked)

	linkedInfo, err := Resolve(linked)
	if err != nil {
		t.Fatalf("Resolve(linked) error = %v", err)
	}
	if _, err := EnsureStream(linkedInfo.PrivateGitDir); err != nil {
		t.Fatalf("EnsureStream() error = %v", err)
	}
	tokenPath := filepath.Join(linkedInfo.PrivateGitDir, streamTokenFile)
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("token should exist before removal: %v", err)
	}

	run(t, primary, "git", "worktree", "remove", "--force", linked)

	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("`git worktree remove` must take the identity with it; Stat error = %v", err)
	}
}

// TestConcurrentFirstMutationsAgreeOnOneToken covers the race a read-then-write
// implementation would lose: several mutating commands starting together in one
// fresh checkout all observe "absent" and all mint. Exactly one token may
// survive, and every caller must be told about that one — a caller that returned
// its own discarded candidate would stamp events with an identity no other
// command in the checkout agrees with.
func TestConcurrentFirstMutationsAgreeOnOneToken(t *testing.T) {
	repo := litRepoWithCommit(t)
	info, err := Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	const racers = 12
	tokens := make([]string, racers)
	errs := make([]error, racers)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			id, err := EnsureStream(info.PrivateGitDir)
			tokens[i], errs[i] = id.Value(), err
		}()
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: EnsureStream() error = %v", i, err)
		}
	}
	for i, token := range tokens {
		if token != tokens[0] {
			t.Fatalf("racer %d disagreed about the checkout's identity: %q vs %q", i, token, tokens[0])
		}
	}
	onDisk, err := ReadStream(info.PrivateGitDir)
	if err != nil {
		t.Fatalf("ReadStream() error = %v", err)
	}
	if onDisk.Value() != tokens[0] {
		t.Fatalf("callers agreed on %q but the file holds %q", tokens[0], onDisk.Value())
	}
}

// TestCorruptTokenIsLoudAndDistinctFromAbsence pins the distinction that a
// success-shaped fallback would destroy. "No file" is a real state with a real
// meaning; "a file that is not a token" is damage. Collapsing the second onto
// the first would report a checkout whose identity was truncated or overwritten
// as a pristine one, and it would then silently mint a SECOND identity for a
// checkout that has already stamped work with its first.
func TestCorruptTokenIsLoudAndDistinctFromAbsence(t *testing.T) {
	valid := strings.Repeat("a", streamTokenLen)
	corrupt := map[string]string{
		"empty file":       "",
		"whitespace only":  "   \n",
		"truncated write":  valid[:streamTokenLen-1],
		"overlong":         valid + "a",
		"outside alphabet": strings.Repeat("z", streamTokenLen-1) + "1",
		"uppercase":        strings.ToUpper(valid),
		"editor text":      "stream id for my laptop",
	}
	for name, contents := range corrupt {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, streamTokenFile), []byte(contents), 0o644); err != nil {
				t.Fatalf("WriteFile error = %v", err)
			}
			id, err := ReadStream(dir)
			if err == nil {
				t.Fatalf("corrupt token %q must fail loudly; got id %q", contents, id.Value())
			}
			if id.Present() {
				t.Fatalf("a failed read must not yield a usable identity; got %q", id.Value())
			}
		})
	}

	// The same call on a directory with no token file is silent and absent.
	id, err := ReadStream(t.TempDir())
	if err != nil {
		t.Fatalf("a missing token file is not an error: %v", err)
	}
	if id.Present() {
		t.Fatalf("expected absence, got %q", id.Value())
	}
}

// TestMintedTokensAreAlwaysAcceptedByTheParser holds the producer and the parser
// to the same shape. They are two descriptions of one token format, and a format
// whose own output its reader rejects would strand a checkout: it would mint an
// identity on first mutation and then fail every command afterwards.
func TestMintedTokensAreAlwaysAcceptedByTheParser(t *testing.T) {
	seen := make(map[string]bool)
	for range 500 {
		token, err := newStreamToken()
		if err != nil {
			t.Fatalf("newStreamToken() error = %v", err)
		}
		parsed, err := parseStreamToken("test", token)
		if err != nil {
			t.Fatalf("parser rejected a freshly minted token %q: %v", token, err)
		}
		if parsed.Value() != token {
			t.Fatalf("round trip changed the token: minted %q, parsed %q", token, parsed.Value())
		}
		seen[token] = true
	}
	if len(seen) != 500 {
		t.Fatalf("tokens must be unique per mint; got %d distinct out of 500", len(seen))
	}
}

// TestTokenCarriesNothingIdentifying enforces the design's privacy invariant at
// the one place it can still be enforced cheaply — the token is about to travel
// into a database that syncs to shared remotes and that this project publishes.
// The check is deliberately coarse: it does not prove the token is
// information-free, it proves the obvious leaks (the checkout's own path, the
// user running it) are absent, which is what a future change to the minting
// scheme would most plausibly break.
func TestTokenCarriesNothingIdentifying(t *testing.T) {
	repo := litRepoWithCommit(t)
	info, err := Resolve(repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	id, err := EnsureStream(info.PrivateGitDir)
	if err != nil {
		t.Fatalf("EnsureStream() error = %v", err)
	}

	token := id.Value()
	leaks := map[string]string{
		"repository path": info.RootDir,
		"repository name": filepath.Base(info.RootDir),
		"username":        os.Getenv("USER"),
		"home directory":  os.Getenv("HOME"),
	}
	for label, secret := range leaks {
		if secret == "" {
			continue
		}
		if strings.Contains(strings.ToLower(token), strings.ToLower(secret)) {
			t.Fatalf("token %q leaks the %s (%q) into the shared database", token, label, secret)
		}
	}
	if len(token) != streamTokenLen {
		t.Fatalf("token %q has length %d, want %d", token, len(token), streamTokenLen)
	}
}
