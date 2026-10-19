package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/types"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestTwoCheckoutsSyncAcrossTheHistoryBound is links-storage-3k6u's
// checkpoint: alpha and bravo share one git remote and take turns writing,
// each write pushed by the writer's on-change mirror and taken in by the
// other's background receive, until the history has crossed the bound many
// times over. Then each edits a different field of one ticket before either
// syncs. Nothing runs but those writes and the two automatic sync paths. They
// run in-process, so the shrunk bound applies and every step lands before the
// next: a detached worker would open its store under the default bound.
//
// The 10x case is the 1x case with ten times the rounds and nothing else
// changed.
func TestTwoCheckoutsSyncAcrossTheHistoryBound(t *testing.T) {
	const rounds = 3
	for _, scale := range []int{1, 10} {
		t.Run(fmt.Sprintf("%dx", scale), func(t *testing.T) {
			if scale > 1 && testing.Short() {
				t.Skip("scale test: ten times the 1x case's writes, under the same logic")
			}
			driveTheBound(t, scale*rounds)
		})
	}
}

// checkpointBound is the bound both cases run under. Its period, the pushes
// from one re-root to the next, is odd, so the two checkouts' alternating
// pushes put each re-root on the other checkout from the one before it.
var checkpointBound = struct{ ReRootAt, Keep uint64 }{ReRootAt: 7, Keep: 2}

// boundCheckout is one side of the checkpoint: its directory, where its owner
// notifications go, and the re-roots its bound published and its receive
// followed.
type boundCheckout struct {
	name, dir, sink     string
	published, followed []string
}

func driveTheBound(t *testing.T, rounds int) {
	r, base := newSharedCheckouts(t)
	finishSharedCheckouts(t, r, base)
	defaultBound := store.DefaultHistoryBound
	t.Cleanup(func() { store.DefaultHistoryBound = defaultBound })
	store.DefaultHistoryBound.ReRootAt = checkpointBound.ReRootAt
	store.DefaultHistoryBound.Keep = checkpointBound.Keep
	remote := filepath.Join(base, "remote.git")

	alpha := &boundCheckout{name: "alpha", dir: r.alpha, sink: filepath.Join(base, "alpha-notifications")}
	bravo := &boundCheckout{name: "bravo", dir: r.bravo, sink: r.sink}
	for round := range rounds {
		for _, turn := range [][2]*boundCheckout{{alpha, bravo}, {bravo, alpha}} {
			writer, reader := turn[0], turn[1]
			runCLIInDir(t, writer.dir, "update", r.epic, "--description", fmt.Sprintf("%s-round-%d", writer.name, round))
			if !writer.mirror(t, remote) {
				t.Fatalf("%s's mirror push in round %d did not land", writer.name, round)
			}
			reader.receive(t)
			for _, side := range []*boundCheckout{writer, reader} {
				side.assertUnderTheBound(t)
			}
		}
	}
	for _, side := range [][2]*boundCheckout{{alpha, bravo}, {bravo, alpha}} {
		followedTheOther := slices.ContainsFunc(side[1].published, func(head string) bool { return slices.Contains(side[0].followed, head) })
		if len(side[0].published) == 0 || !followedTheOther {
			t.Fatalf("%s published re-roots %v and followed %v; %s published %v: want each checkout to publish a re-root and follow one the other published", side[0].name, side[0].published, side[0].followed, side[1].name, side[1].published)
		}
	}

	// The concurrent edit: neither side syncs between the two writes. alpha's
	// push lands first, so the remote refuses bravo's until bravo's receive
	// has merged alpha's edit, and bravo's next write pushes the merge.
	runCLIInDir(t, r.alpha, "update", r.ticket, "--description", "alpha-description")
	runCLIInDir(t, r.bravo, "update", r.ticket, "--title", "bravo-title")
	if !alpha.mirror(t, remote) {
		t.Fatal("alpha's mirror push of the concurrent edit did not land")
	}
	if bravo.mirror(t, remote) {
		t.Fatal("bravo's mirror push landed over alpha's; want the remote to refuse it")
	}
	bravo.receive(t)
	runCLIInDir(t, r.bravo, "update", r.epic, "--description", "bravo-after-the-merge")
	if !bravo.mirror(t, remote) {
		t.Fatal("bravo's mirror push of the merge did not land")
	}
	alpha.receive(t)
	for _, side := range []*boundCheckout{alpha, bravo} {
		if show := runCLIInDir(t, side.dir, "show", r.ticket); !strings.Contains(show, "alpha-description") || !strings.Contains(show, "bravo-title") {
			t.Fatalf("%s's ticket after the concurrent edit:\n%s\nwant alpha's description and bravo's title", side.name, show)
		}
		side.assertUnderTheBound(t)
		side.assertNothingEscalated(t)
	}
	alphaHead, _ := syncHeadAndBranch(t, r.alpha)
	if bravoHead, _ := syncHeadAndBranch(t, r.bravo); alphaHead != bravoHead {
		t.Fatalf("after both received, alpha is at %s and bravo at %s", alphaHead, bravoHead)
	}

	// A checkout that adopts the remote now finds only the window there.
	runGit(t, base, "clone", remote, "gamma")
	gamma := &boundCheckout{name: "gamma", dir: filepath.Join(base, "gamma")}
	runCLIInDir(t, gamma.dir, "init", "--skip-hooks", "--skip-agents")
	gamma.assertUnderTheBound(t)
}

// mirror runs c's on-change mirror and reports whether its push landed: a
// landed push keeps the history bound, and a refused one does not. The bound
// must keep the history without error, and once it fired, publish its
// re-root under a held lease, after which the remote's GC leaves git's tree
// holding exactly the bytes it collected.
func (c *boundCheckout) mirror(t *testing.T, remote string) (landed bool) {
	t.Helper()
	runCLIInDir(t, c.dir, "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
	facts := lastEvent(t).Facts
	if records, _ := facts["history_bound.records"].([]any); len(records) == 0 {
		return false
	}
	bound := boundRecordOf(t, facts)
	if bound["history_bound.error"] != nil {
		t.Fatalf("%s's mirror bound = %v; want no error", c.name, bound)
	}
	if bound["history_bound.fired"] != true {
		return true
	}
	if bound["history_bound.lease"] != "held" || bound["remote_gc.outcome"] != "replaced" {
		t.Fatalf("%s's mirror bound = %v; want its re-root published under a held lease and the remote's table files replaced", c.name, bound)
	}
	if collected, inTree := bound["remote_gc.collected_bytes"], float64(remoteDataBytes(t, remote)); collected != inTree {
		t.Fatalf("%s's bound collected %v bytes of the remote; git counts %v in its refs/dolt/data tree", c.name, collected, inTree)
	}
	c.published = append(c.published, bound["reroot.root"].(string))
	return true
}

// receive runs c's background receive and records the re-root it followed,
// if it followed one.
func (c *boundCheckout) receive(t *testing.T) {
	t.Helper()
	runCLIInDir(t, c.dir, "sync", backgroundReceiveSubcommand, "--parent-pid", "0")
	follows, _ := lastEvent(t).Facts["reroot.follows"].([]any)
	for _, entry := range follows {
		follow, _ := entry.(map[string]any)
		if follow["reroot.follow"] != "receive" {
			t.Fatalf("%s's receive followed %v; want follow=receive", c.name, follow)
		}
		c.followed = append(c.followed, follow["reroot.root"].(string))
	}
}

// assertUnderTheBound checks c's sync branch head sits below the height at
// which a push re-roots.
func (c *boundCheckout) assertUnderTheBound(t *testing.T) {
	t.Helper()
	if height := walkedHeight(t, c.dir); height >= checkpointBound.ReRootAt {
		t.Fatalf("%s's head is at height %d; want it under the bound's %d", c.name, height, checkpointBound.ReRootAt)
	}
}

// assertNothingEscalated checks no sync of c's surfaced the remote as
// unrelated, left a receive block pending, or notified the owner, and lit
// doctor finds nothing unrelated or diverged.
func (c *boundCheckout) assertNothingEscalated(t *testing.T) {
	t.Helper()
	ws, err := workspace.Resolve(c.dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", c.name, err)
	}
	for _, record := range readSyncTraces(t, ws) {
		if record.Decision == string(syncFailureUnrelatedHistories) {
			t.Fatalf("%s's sync trace %+v; want no unrelated histories", c.name, record)
		}
	}
	if _, err := os.Stat(receiveBlockPendingPath(ws)); !os.IsNotExist(err) {
		t.Fatalf("a receive block is pending in %s (stat err=%v)", c.name, err)
	}
	if payload, err := os.ReadFile(c.sink); !os.IsNotExist(err) {
		t.Fatalf("%s's owner was notified: %q (err=%v)", c.name, payload, err)
	}
	if doctor := runCLIInDir(t, c.dir, "doctor"); strings.Contains(doctor, "unrelated") || strings.Contains(doctor, "diverged") {
		t.Fatalf("lit doctor in %s:\n%s", c.name, doctor)
	}
}

// walkedHeight is the height of dir's sync branch head, the commits on the
// longest line from it down to a root, itself counted, found by walking
// parent links in the checkout's chunk store. The bound reads the height from
// dolt_log's commit_order; this reads it a second way.
func walkedHeight(t *testing.T, dir string) uint64 {
	t.Helper()
	ctx := context.Background()
	ws, err := workspace.Resolve(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	_, branch := syncHeadAndBranch(t, dir)
	nomsDir := filepath.Join(ws.DoltRepoPath, dbfactory.DoltDataDir)
	params := map[string]any{dbfactory.ChunkJournalParam: struct{}{}, dbfactory.DisableSingletonCacheParam: struct{}{}}
	ddb, err := doltdb.LoadDoltDBWithParams(ctx, types.Format_Default, "file://"+filepath.ToSlash(nomsDir), filesys.LocalFS, params)
	if err != nil {
		t.Fatalf("open %s's chunk store: %v", dir, err)
	}
	defer func() {
		if err := ddb.Close(); err != nil {
			t.Fatalf("close %s's chunk store: %v", dir, err)
		}
	}()
	head, err := ddb.ResolveCommitRef(ctx, ref.NewBranchRef(branch))
	if err != nil {
		t.Fatalf("resolve %s's branch %s: %v", dir, branch, err)
	}
	addr, err := head.HashOf()
	if err != nil {
		t.Fatal(err)
	}
	heights := map[hash.Hash]uint64{}
	var heightOf func(addr hash.Hash) uint64
	heightOf = func(addr hash.Hash) uint64 {
		if h, ok := heights[addr]; ok {
			return h
		}
		optional, err := ddb.ReadCommit(ctx, addr)
		if err != nil {
			t.Fatalf("read commit %s in %s: %v", addr, dir, err)
		}
		commit, ok := optional.ToCommit()
		if !ok {
			t.Fatalf("%s holds commit %s only as a ghost", dir, addr)
		}
		parents, err := commit.ParentHashes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var below uint64
		for _, parent := range parents {
			below = max(below, heightOf(parent))
		}
		heights[addr] = below + 1
		return below + 1
	}
	return heightOf(addr)
}

// remoteDataBytes is the bytes of the table files in the tree the remote's
// refs/dolt/data names, read with git, the manifest left out. A table file
// larger than a blob is a directory of parts, and its parts are summed.
func remoteDataBytes(t *testing.T, remote string) int64 {
	t.Helper()
	tree, err := exec.Command("git", "--git-dir", remote, "ls-tree", "-r", "--long", "refs/dolt/data").Output()
	if err != nil {
		t.Fatalf("list the remote's refs/dolt/data tree: %v", err)
	}
	var bytes int64
	for _, line := range strings.Split(strings.TrimSpace(string(tree)), "\n") {
		meta, path, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("ls-tree line %q has no path", line)
		}
		if name, _, _ := strings.Cut(path, "/"); name == "manifest" {
			continue
		}
		fields := strings.Fields(meta)
		size, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if err != nil {
			t.Fatalf("ls-tree line %q: %v", line, err)
		}
		bytes += size
	}
	return bytes
}
