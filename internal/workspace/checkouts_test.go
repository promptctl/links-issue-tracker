package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tokenOf mints and returns a checkout's identity, the way a first mutating
// command in it would.
func tokenOf(t *testing.T, worktree string) string {
	t.Helper()
	info, err := Resolve(worktree)
	if err != nil {
		t.Fatalf("Resolve(%q) error = %v", worktree, err)
	}
	id, err := EnsureStream(info.PrivateGitDir)
	if err != nil {
		t.Fatalf("EnsureStream(%q) error = %v", worktree, err)
	}
	return id.Value()
}

// liveTokens is the enumeration reduced to the only thing the claim predicate
// compares: the set of tokens belonging to checkouts that still exist.
func liveTokens(t *testing.T, cwd string) []string {
	t.Helper()
	checkouts, err := LiveCheckouts(cwd)
	if err != nil {
		t.Fatalf("LiveCheckouts(%q) error = %v", cwd, err)
	}
	var tokens []string
	for _, checkout := range checkouts {
		if checkout.Stream.Present() {
			tokens = append(tokens, checkout.Stream.Value())
		}
	}
	return tokens
}

// TestLiveCheckoutsPairsEveryWorktreeWithItsOwnToken is the enumeration's
// contract: every working tree of the repository, each carrying the identity
// that checkout minted and no other's.
//
// The third worktree is the "absent is not dead" case, and it is here rather
// than in its own test because it is only meaningful alongside the others: a
// checkout that has only ever been read holds no token, is still enumerated as
// live, and contributes nothing to the token set. Reading its absence as
// deletion would be the same mistake in the opposite direction.
func TestLiveCheckoutsPairsEveryWorktreeWithItsOwnToken(t *testing.T) {
	primary := litRepoWithCommit(t)
	mutated := filepath.Join(t.TempDir(), "mutated")
	readOnly := filepath.Join(t.TempDir(), "read-only")
	run(t, primary, "git", "worktree", "add", mutated)
	run(t, primary, "git", "worktree", "add", readOnly)

	primaryToken := tokenOf(t, primary)
	mutatedToken := tokenOf(t, mutated)

	checkouts, err := LiveCheckouts(primary)
	if err != nil {
		t.Fatalf("LiveCheckouts() error = %v", err)
	}
	if len(checkouts) != 3 {
		t.Fatalf("enumerated %d checkouts, want 3: %+v", len(checkouts), checkouts)
	}

	tokens := liveTokens(t, primary)
	slices.Sort(tokens)
	want := []string{primaryToken, mutatedToken}
	slices.Sort(want)
	if !slices.Equal(tokens, want) {
		t.Fatalf("live tokens = %v, want exactly the two minted ones %v — a never-mutated checkout holds no identity and must contribute none", tokens, want)
	}
	if primaryToken == mutatedToken {
		t.Fatal("two worktrees shared one token; the enumeration cannot tell them apart")
	}
}

// TestLiveCheckoutsCarriesTheAddress pins the second thing the enumeration is
// for: a claimant that is a live worktree of this store on this machine can be
// walked over to, and this is where its path and branch come from. Rendering
// them is the visibility ticket's job; producing them without storing them is
// this one's.
func TestLiveCheckoutsCarriesTheAddress(t *testing.T) {
	primary := litRepoWithCommit(t)
	onBranch := filepath.Join(t.TempDir(), "on-branch")
	detached := filepath.Join(t.TempDir(), "detached")
	run(t, primary, "git", "worktree", "add", "-b", "side-quest", onBranch)
	run(t, primary, "git", "worktree", "add", "--detach", detached)

	checkouts, err := LiveCheckouts(primary)
	if err != nil {
		t.Fatalf("LiveCheckouts() error = %v", err)
	}
	addresses := map[string]string{}
	for _, checkout := range checkouts {
		resolved, err := filepath.EvalSymlinks(checkout.Path)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q) error = %v", checkout.Path, err)
		}
		addresses[resolved] = checkout.Branch
	}

	branchPath, err := filepath.EvalSymlinks(onBranch)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	if got, ok := addresses[branchPath]; !ok || got != "side-quest" {
		t.Fatalf("branch at %q = %q (found %v), want %q rendered short", branchPath, got, ok, "side-quest")
	}

	detachedPath, err := filepath.EvalSymlinks(detached)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	got, ok := addresses[detachedPath]
	if !ok {
		t.Fatalf("detached worktree %q missing from the enumeration: %v", detachedPath, addresses)
	}
	if got != "" {
		t.Fatalf("detached HEAD rendered branch %q; a detached checkout is on no branch and must say so by carrying none", got)
	}
}

// TestRemovedWorktreeIsGoneFromTheEnumeration is the acceptance at this layer:
// `git worktree remove` takes the checkout's identity with the directory, and
// the very next enumeration no longer counts it live. Nothing of lit's runs to
// make that happen.
func TestRemovedWorktreeIsGoneFromTheEnumeration(t *testing.T) {
	primary := litRepoWithCommit(t)
	doomed := filepath.Join(t.TempDir(), "doomed")
	run(t, primary, "git", "worktree", "add", doomed)

	survivor := tokenOf(t, primary)
	condemned := tokenOf(t, doomed)
	if !slices.Contains(liveTokens(t, primary), condemned) {
		t.Fatal("the worktree was not live before it was removed; the removal below proves nothing")
	}

	run(t, primary, "git", "worktree", "remove", "--force", doomed)

	after := liveTokens(t, primary)
	if slices.Contains(after, condemned) {
		t.Fatalf("removed worktree's token %q is still live: %v", condemned, after)
	}
	if !slices.Contains(after, survivor) {
		t.Fatalf("the surviving checkout's token %q vanished with its neighbor: %v", survivor, after)
	}
}

// TestDeletedWorktreeDirectoryIsNotLiveThoughItsTokenSurvives is the reason this
// asks git instead of listing <git-common-dir>/worktrees.
//
// A worktree deleted with `rm -rf` rather than `git worktree remove` leaves its
// private git directory — token and all — sitting under the common dir until
// somebody prunes. The test asserts that file is still there, so an
// implementation that enumerated the directory would pass a liveness check for a
// checkout that has been gone for as long as anyone cares to leave it. Git calls
// the record prunable; that judgment is the whole answer.
func TestDeletedWorktreeDirectoryIsNotLiveThoughItsTokenSurvives(t *testing.T) {
	primary := litRepoWithCommit(t)
	deleted := filepath.Join(t.TempDir(), "deleted")
	run(t, primary, "git", "worktree", "add", deleted)

	orphan := tokenOf(t, deleted)
	info, err := Resolve(deleted)
	if err != nil {
		t.Fatalf("Resolve(deleted) error = %v", err)
	}
	tokenPath := filepath.Join(info.PrivateGitDir, streamTokenFile)

	if err := os.RemoveAll(deleted); err != nil {
		t.Fatalf("RemoveAll(%q) error = %v", deleted, err)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("this test is only meaningful while the orphaned token survives at %s: %v", tokenPath, err)
	}

	if live := liveTokens(t, primary); slices.Contains(live, orphan) {
		t.Fatalf("token %q of a deleted worktree is still live: %v — the enumeration is reading the filesystem, not asking git", orphan, live)
	}
}

// TestLockedWorktreeIsNeverDeclaredDead holds the edge in the safe direction. A
// locked worktree whose directory is currently missing — removable media,
// an unmounted share — is one git deliberately refuses to call prunable. This
// machine cannot reach its identity, and the honest report is a loud failure to
// enumerate, never a quiet "not in the live set" that would void a claimant
// whose only offense is being unplugged.
func TestLockedWorktreeIsNeverDeclaredDead(t *testing.T) {
	primary := litRepoWithCommit(t)
	unplugged := filepath.Join(t.TempDir(), "unplugged")
	run(t, primary, "git", "worktree", "add", unplugged)
	tokenOf(t, unplugged)

	run(t, primary, "git", "worktree", "lock", unplugged)
	if err := os.RemoveAll(unplugged); err != nil {
		t.Fatalf("RemoveAll(%q) error = %v", unplugged, err)
	}

	_, err := LiveCheckouts(primary)
	if err == nil {
		t.Fatal("a checkout git lists as live but whose identity cannot be read must fail the enumeration, not be dropped from it")
	}
	if !strings.Contains(err.Error(), "git lists as live") {
		t.Fatalf("LiveCheckouts() error = %q, want it to name the unreachable-but-live checkout", err)
	}
}

// TestDamagedIdentityInAnotherCheckoutFailsLoudly holds the difference between
// "this checkout has no identity" and "this checkout's identity is unreadable".
// The first is the ordinary never-mutated state and contributes no token; the
// second is damage, and laundering it into the first would let a corrupted file
// in a neighbouring worktree void every claim that worktree holds — the loudest
// possible consequence reached by the quietest possible path.
func TestDamagedIdentityInAnotherCheckoutFailsLoudly(t *testing.T) {
	primary := litRepoWithCommit(t)
	neighbour := filepath.Join(t.TempDir(), "neighbour")
	run(t, primary, "git", "worktree", "add", neighbour)
	tokenOf(t, neighbour)

	info, err := Resolve(neighbour)
	if err != nil {
		t.Fatalf("Resolve(neighbour) error = %v", err)
	}
	tokenPath := filepath.Join(info.PrivateGitDir, streamTokenFile)
	if err := os.WriteFile(tokenPath, []byte("not a token"), 0o644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	if _, err := LiveCheckouts(primary); err == nil {
		t.Fatal("a checkout whose identity file is damaged must fail the enumeration, not be counted as having none")
	} else if !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("LiveCheckouts() error = %q, want the malformed-token diagnosis", err)
	}
}

// TestBareRepositoryContributesNoCheckout covers the other record with no
// working tree. A bare repository can host linked worktrees, and lit cannot even
// resolve a workspace inside one, so it mints no identity and can hold no claim
// — but it does appear in the list, and counting it would put a checkout in the
// enumeration that nobody can ever be standing in.
func TestBareRepositoryContributesNoCheckout(t *testing.T) {
	seed := litRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	run(t, seed, "git", "clone", "--bare", seed, bare)
	linked := filepath.Join(t.TempDir(), "linked")
	run(t, bare, "git", "worktree", "add", linked)

	checkouts, err := LiveCheckouts(linked)
	if err != nil {
		t.Fatalf("LiveCheckouts() error = %v", err)
	}
	if len(checkouts) != 1 {
		t.Fatalf("enumerated %d checkouts, want only the one working tree: %+v", len(checkouts), checkouts)
	}
	resolved, err := filepath.EvalSymlinks(checkouts[0].Path)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	wantPath, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	if resolved != wantPath {
		t.Fatalf("enumerated %q, want the linked worktree %q", resolved, wantPath)
	}
}

// TestParseWorktreeListReadsEveryDocumentedShape walks the record grammar one
// row at a time, because the cost of misreading any single attribute is a live
// checkout counted dead or a deleted one counted live.
func TestParseWorktreeListReadsEveryDocumentedShape(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   []worktreeRecord
	}{
		{
			name:   "a branch ref renders short",
			output: "worktree /w\nHEAD abc\nbranch refs/heads/feature/nested\n",
			want:   []worktreeRecord{{path: "/w", branch: "feature/nested"}},
		},
		{
			name:   "detached leaves no branch",
			output: "worktree /w\nHEAD abc\ndetached\n",
			want:   []worktreeRecord{{path: "/w"}},
		},
		{
			name:   "prunable with a reason",
			output: "worktree /w\nHEAD abc\nbranch refs/heads/gone\nprunable gitdir file points to non-existent location\n",
			want:   []worktreeRecord{{path: "/w", branch: "gone", prunable: true}},
		},
		{
			name:   "prunable with no reason",
			output: "worktree /w\nHEAD abc\nprunable\n",
			want:   []worktreeRecord{{path: "/w", prunable: true}},
		},
		{
			name:   "locked is not prunable",
			output: "worktree /w\nHEAD abc\nbranch refs/heads/held\nlocked on a usb stick\n",
			want:   []worktreeRecord{{path: "/w", branch: "held"}},
		},
		{
			name:   "a bare repository",
			output: "worktree /repo.git\nbare\n",
			want:   []worktreeRecord{{path: "/repo.git", bare: true}},
		},
		{
			name:   "an attribute git has not invented yet is ignored",
			output: "worktree /w\nHEAD abc\nbranch refs/heads/main\nquarantined by a future release\n",
			want:   []worktreeRecord{{path: "/w", branch: "main"}},
		},
		{
			name:   "a path containing spaces survives intact",
			output: "worktree /some where/my worktree\nHEAD abc\ndetached\n",
			want:   []worktreeRecord{{path: "/some where/my worktree"}},
		},
		{
			name:   "several records separated by blank lines",
			output: "worktree /a\nHEAD abc\nbranch refs/heads/main\n\nworktree /b\nHEAD def\nprunable\n\nworktree /c\nHEAD ghi\ndetached\n",
			want: []worktreeRecord{
				{path: "/a", branch: "main"},
				{path: "/b", prunable: true},
				{path: "/c"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWorktreeList(tc.output)
			if err != nil {
				t.Fatalf("parseWorktreeList() error = %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("parseWorktreeList() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestParseWorktreeListRefusesOutputThatIsNotTheFormat pins the two ways the
// answer can fail to be an answer. Both matter for the same reason: an
// enumeration that silently yields no live checkouts is not "no worktrees" — it
// is this machine voiding every claim its workspace holds.
func TestParseWorktreeListRefusesOutputThatIsNotTheFormat(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{name: "empty output", output: ""},
		{name: "whitespace only", output: "\n\n"},
		{name: "an attribute before any worktree line", output: "HEAD abc\nworktree /w\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWorktreeList(tc.output)
			if err == nil {
				t.Fatalf("parseWorktreeList(%q) = %+v, want an error", tc.output, got)
			}
		})
	}
}

// TestUninhabitedCoversBothRecordsWithNoWorkingTree states the filter as a truth
// table, so a record kind added to worktreeRecord without a decision here shows
// up as a missing row rather than as a default answer.
func TestUninhabitedCoversBothRecordsWithNoWorkingTree(t *testing.T) {
	cases := []struct {
		record worktreeRecord
		want   bool
	}{
		{record: worktreeRecord{path: "/w", branch: "main"}, want: false},
		{record: worktreeRecord{path: "/w"}, want: false},
		{record: worktreeRecord{path: "/w", prunable: true}, want: true},
		{record: worktreeRecord{path: "/w", bare: true}, want: true},
	}
	for _, tc := range cases {
		if got := tc.record.uninhabited(); got != tc.want {
			t.Errorf("worktreeRecord%+v.uninhabited() = %v, want %v", tc.record, got, tc.want)
		}
	}
}
