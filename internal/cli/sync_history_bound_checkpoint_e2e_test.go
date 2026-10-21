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
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/types"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestTwoCheckoutsSyncAcrossTheHistoryBound is links-storage-3k6u's
// checkpoint: alpha and bravo share one git remote and take turns writing,
// each write pushed by the writer's on-change mirror and taken in by the
// other's background receive, until the history has crossed the bound many
// times over. Then each edits a different field of one ticket before either
// syncs, and the push that lands first re-roots. Nothing runs but those
// writes and the two automatic sync paths. They run in-process, so every step
// lands before the next.
//
// The 10x case is the 1x case with ten times the rounds and nothing else
// changed.
func TestTwoCheckoutsSyncAcrossTheHistoryBound(t *testing.T) {
	for _, scale := range []int{1, 10} {
		t.Run(fmt.Sprintf("%dx", scale), func(t *testing.T) {
			if scale > 1 && testing.Short() {
				t.Skip("scale test: ten times the 1x case's writes, under the same logic")
			}
			driveTheBound(t, scale*checkpointPeriod)
		})
	}
}

// The bound both cases run under. A re-root leaves the remote head at height
// checkpointKeep and each push adds one, so a re-root comes every
// checkpointPeriod pushes. The period is odd, so the two checkouts'
// alternating pushes put each re-root on the other checkout from the one
// before it, and rounds that are a whole number of periods put the
// concurrent edit's push on a re-root at every scale.
const (
	checkpointReRootAt = 5
	checkpointKeep     = 2
	checkpointPeriod   = checkpointReRootAt - checkpointKeep
)

// boundCheckout is one side of the checkpoint: its directory, workspace and
// sync branch, where its owner notifications go, and the re-roots its bound
// published and its receive followed.
type boundCheckout struct {
	name, dir, sink, branch string
	ws                      workspace.Info
	published, followed     []string
}

func newBoundCheckout(t *testing.T, name, dir, sink string) *boundCheckout {
	t.Helper()
	ws, err := workspace.Resolve(testEnv(), dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	_, branch := syncHeadAndBranch(t, dir)
	return &boundCheckout{name: name, dir: dir, sink: sink, branch: branch, ws: ws}
}

func driveTheBound(t *testing.T, rounds int) {
	r, base := newSharedCheckouts(t)
	finishSharedCheckouts(t, r, base)
	for _, repo := range []string{r.alpha, r.bravo} {
		setHistoryWindow(t, repo, checkpointReRootAt, checkpointKeep)
	}
	remote := filepath.Join(base, "remote.git")
	alpha := newBoundCheckout(t, "alpha", r.alpha, filepath.Join(base, "alpha-notifications"))
	bravo := newBoundCheckout(t, "bravo", r.bravo, r.sink)
	historyBefore := historyBytes(t, remote, alpha, "before the first re-root")
	// The seeded history is above the bound, so the first push re-roots, and
	// every checkpointPeriod-th push after it.
	pushes := 0
	landOnSchedule := func(side *boundCheckout, what string) {
		t.Helper()
		push := side.mirror(t, remote)
		if want := pushes%checkpointPeriod == 0; push.decision != pushDecisionPushed || push.reRooted != want {
			t.Fatalf("%s's mirror push %d (%s): %v; want decision %s, re-rooted %v", side.name, pushes, what, push, pushDecisionPushed, want)
		}
		pushes++
	}
	for round := range rounds {
		for _, turn := range [][2]*boundCheckout{{alpha, bravo}, {bravo, alpha}} {
			writer, reader := turn[0], turn[1]
			runCLIInDir(t, writer.dir, "update", r.epic, "--description", fmt.Sprintf("%s-round-%d", writer.name, round))
			landOnSchedule(writer, fmt.Sprintf("round %d", round))
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
	// push lands first and re-roots, so the remote refuses bravo's until
	// bravo's receive has followed the re-root and merged alpha's edit, and
	// bravo's next write pushes the merge.
	runCLIInDir(t, r.alpha, "update", r.ticket, "--description", "alpha-description")
	runCLIInDir(t, r.bravo, "update", r.ticket, "--title", "bravo-title")
	landOnSchedule(alpha, "the concurrent edit")
	if push := bravo.mirror(t, remote); push.decision != pushDecisionDiverged {
		t.Fatalf("bravo's mirror push of the concurrent edit: %v; want decision %s, the remote refusing it over alpha's", push, pushDecisionDiverged)
	}
	bravo.receive(t)
	runCLIInDir(t, r.bravo, "update", r.epic, "--description", "bravo-after-the-merge")
	if push := bravo.mirror(t, remote); push.decision != pushDecisionPushed {
		t.Fatalf("bravo's mirror push of the merge: %v; want decision %s", push, pushDecisionPushed)
	}
	alpha.receive(t)
	for _, side := range []*boundCheckout{alpha, bravo} {
		if show := runCLIInDir(t, side.dir, "show", r.ticket); !strings.Contains(show, "alpha-description") || !strings.Contains(show, "bravo-title") {
			t.Fatalf("%s's ticket after the concurrent edit:\n%s\nwant alpha's description and bravo's title", side.name, show)
		}
		side.assertAtTheRemote(t)
		side.assertUnderTheBound(t)
		side.assertNothingEscalated(t)
	}
	if after := historyBytes(t, remote, alpha, fmt.Sprintf("after %d rounds", rounds)); after.history() > historyBefore.history() {
		t.Fatalf("the remote holds %s; %s: want no more history than before", after, historyBefore)
	}

	// A checkout that adopts the remote now adopts the re-rooted history.
	runGit(t, base, "clone", remote, "gamma")
	gammaDir := filepath.Join(base, "gamma")
	runCLIInDir(t, gammaDir, "init", "--skip-hooks", "--skip-agents")
	newBoundCheckout(t, "gamma", gammaDir, "").assertUnderTheBound(t)
}

// mirrorPush is one on-change mirror's push: its decision and, for a push
// that landed, whether its bound re-rooted. It carries the event's
// push.outcomes entry and history_bound.records whole, so a caller whose want
// misses prints why the push ended and why its bound did or did not fire.
type mirrorPush struct {
	decision string
	reRooted bool
	outcome  map[string]any
	bounds   []any
}

func (p mirrorPush) String() string {
	return fmt.Sprintf("decision %s, re-rooted %v; push outcome %v; history bound %v", p.decision, p.reRooted, p.outcome, p.bounds)
}

// mirror runs c's on-change mirror and returns its push. A landed push's
// bound keeps the history without error. A re-root is published under a held
// lease, after which the remote's GC leaves git's tree holding exactly the
// bytes it collected.
func (c *boundCheckout) mirror(t *testing.T, remote string) mirrorPush {
	t.Helper()
	runCLIInDir(t, c.dir, "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
	ev := lastEvent(t)
	outcomes := ev.Occurrences["push.outcomes"]
	if len(outcomes) != 1 {
		t.Fatalf("%s's mirror push.outcomes = %v; want the one attempt", c.name, outcomes)
	}
	outcome, _ := outcomes[0].(map[string]any)
	push := mirrorPush{decision: stringFact(t, outcome, "push.decision"), outcome: outcome, bounds: ev.Occurrences["history_bound.records"]}
	if push.decision != pushDecisionPushed {
		return push
	}
	bound := boundRecordOf(t, ev)
	if bound["history_bound.error"] != nil {
		t.Fatalf("%s's mirror push: %v; want its bound to end without error", c.name, push)
	}
	if bound["history_bound.fired"] != true {
		return push
	}
	if bound["history_bound.lease"] != "held" || bound["remote_gc.outcome"] != "replaced" {
		t.Fatalf("%s's mirror push: %v; want its re-root published under a held lease and the remote's table files replaced", c.name, push)
	}
	if collected, inTree := bound["remote_gc.collected_bytes"], float64(remoteDataBytes(t, remote)); collected != inTree {
		t.Fatalf("%s's bound collected %v bytes of the remote; git counts %v in its refs/dolt/data tree; %v", c.name, collected, inTree, push)
	}
	c.published = append(c.published, stringFact(t, bound, "reroot.root"))
	push.reRooted = true
	return push
}

// receive runs c's background receive and records the re-root it followed,
// if it followed one.
func (c *boundCheckout) receive(t *testing.T) {
	t.Helper()
	runCLIInDir(t, c.dir, "sync", backgroundReceiveSubcommand, "--parent-pid", "0")
	follows := lastEvent(t).Occurrences["reroot.follows"]
	for _, entry := range follows {
		follow, _ := entry.(map[string]any)
		if follow["reroot.follow"] != "receive" {
			t.Fatalf("%s's receive followed %v; want follow=receive", c.name, follow)
		}
		c.followed = append(c.followed, stringFact(t, follow, "reroot.root"))
	}
}

// stringFact is record's key, which the test fails without.
func stringFact(t *testing.T, record map[string]any, key string) string {
	t.Helper()
	value, ok := record[key].(string)
	if !ok {
		t.Fatalf("record %v has no string %s", record, key)
	}
	return value
}

// assertUnderTheBound checks c's sync branch head sits below the height at
// which a push re-roots.
func (c *boundCheckout) assertUnderTheBound(t *testing.T) {
	t.Helper()
	if height := c.walkedHeight(t); height >= checkpointReRootAt {
		t.Fatalf("%s's head is at height %d; want it under the bound's %d", c.name, height, checkpointReRootAt)
	}
}

// assertNothingEscalated checks no sync of c's surfaced the remote as
// unrelated, left a receive block pending, or notified the owner, and lit
// doctor finds nothing unrelated or diverged.
func (c *boundCheckout) assertNothingEscalated(t *testing.T) {
	t.Helper()
	for _, record := range readSyncTraces(t, c.ws) {
		if record.Decision == string(syncFailureUnrelatedHistories) {
			t.Fatalf("%s's sync trace %+v; want no unrelated histories", c.name, record)
		}
	}
	if _, err := os.Stat(receiveBlockPendingPath(c.ws)); !os.IsNotExist(err) {
		t.Fatalf("a receive block is pending in %s (stat err=%v)", c.name, err)
	}
	if payload, err := os.ReadFile(c.sink); !os.IsNotExist(err) {
		t.Fatalf("%s's owner was notified: %q (err=%v)", c.name, payload, err)
	}
	if doctor := runCLIInDir(t, c.dir, "doctor"); strings.Contains(doctor, "unrelated") || strings.Contains(doctor, "diverged") {
		t.Fatalf("lit doctor in %s:\n%s", c.name, doctor)
	}
}

// walkedHeight is the height of c's sync branch head, the commits on the
// longest line from it down to a root, itself counted, found by walking
// parent links in the checkout's chunk store. The bound reads the height from
// dolt_log's commit_order; this reads it a second way.
func (c *boundCheckout) walkedHeight(t *testing.T) (height uint64) {
	t.Helper()
	c.atSyncHead(t, func(ctx context.Context, ddb *doltdb.DoltDB, head *doltdb.Commit) {
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
				t.Fatalf("read commit %s in %s: %v", addr, c.name, err)
			}
			commit, ok := optional.ToCommit()
			if !ok {
				t.Fatalf("%s holds commit %s only as a ghost", c.name, addr)
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
		height = heightOf(addr)
	})
	return height
}

// assertAtTheRemote checks c's sync branch head is the remote's, as c's
// fetch finds it.
func (c *boundCheckout) assertAtTheRemote(t *testing.T) {
	t.Helper()
	runCLIInDir(t, c.dir, "sync", "fetch")
	c.atSyncHead(t, func(ctx context.Context, ddb *doltdb.DoltDB, head *doltdb.Commit) {
		remoteHead, err := ddb.ResolveCommitRef(ctx, ref.NewRemoteRef("origin", c.branch))
		if err != nil {
			t.Fatalf("resolve the remote's %s in %s: %v", c.branch, c.name, err)
		}
		ours, err := head.HashOf()
		if err != nil {
			t.Fatal(err)
		}
		if theirs, err := remoteHead.HashOf(); err != nil || ours != theirs {
			t.Fatalf("%s is at %s and the remote at %s (err=%v); want the remote's head", c.name, ours, theirs, err)
		}
	})
}

// remoteStateBytes is the table-file bytes of the remote's current state
// alone, read through c once c is at the remote's head: the tables that head
// holds, without the commits that lead to them, pulled into a scratch store
// with no chunk journal the way the remote's GC builds the files it collects
// (store.buildCollectedSet). So it counts in the unit remoteDataBytes reads.
func (c *boundCheckout) remoteStateBytes(t *testing.T) (size int64) {
	t.Helper()
	c.assertAtTheRemote(t)
	c.atSyncHead(t, func(ctx context.Context, ddb *doltdb.DoltDB, head *doltdb.Commit) {
		tables, err := head.GetRootValue(ctx)
		if err != nil {
			t.Fatalf("read %s's tables: %v", c.name, err)
		}
		addr, err := tables.HashOf()
		if err != nil {
			t.Fatal(err)
		}
		scratch := t.TempDir()
		nomsDir, tempDir := filepath.Join(scratch, "noms"), filepath.Join(scratch, "tmp")
		for _, dir := range []string{nomsDir, tempDir} {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		state, err := doltdb.LoadDoltDBWithParams(ctx, types.Format_Default, "file://"+filepath.ToSlash(nomsDir), filesys.LocalFS, map[string]any{dbfactory.DisableSingletonCacheParam: struct{}{}})
		if err != nil {
			t.Fatalf("open a scratch store for %s's state: %v", c.name, err)
		}
		defer func() {
			if err := state.Close(); err != nil {
				t.Fatalf("close the scratch store for %s's state: %v", c.name, err)
			}
		}()
		if err := state.PullChunks(ctx, tempDir, ddb, []hash.Hash{addr}, nil, nil); err != nil {
			t.Fatalf("copy %s's tables into a scratch store: %v", c.name, err)
		}
		files, ok := datas.ChunkStoreFromDatabase(doltdb.ExposeDatabaseFromDoltDB(state)).(chunks.TableFileStore)
		if !ok {
			t.Fatalf("the scratch store for %s's state lists no table files", c.name)
		}
		bytes, err := files.Size(ctx)
		if err != nil {
			t.Fatalf("size the scratch store for %s's state: %v", c.name, err)
		}
		size = int64(bytes)
	})
	return size
}

// atSyncHead runs read over c's chunk store, opened outside lit, at c's sync
// branch head.
func (c *boundCheckout) atSyncHead(t *testing.T, read func(ctx context.Context, ddb *doltdb.DoltDB, head *doltdb.Commit)) {
	t.Helper()
	ctx := context.Background()
	nomsDir := filepath.Join(c.ws.DoltRepoPath, dbfactory.DoltDataDir)
	params := map[string]any{dbfactory.ChunkJournalParam: struct{}{}, dbfactory.DisableSingletonCacheParam: struct{}{}}
	ddb, err := doltdb.LoadDoltDBWithParams(ctx, types.Format_Default, "file://"+filepath.ToSlash(nomsDir), filesys.LocalFS, params)
	if err != nil {
		t.Fatalf("open %s's chunk store: %v", c.name, err)
	}
	defer func() {
		if err := ddb.Close(); err != nil {
			t.Fatalf("close %s's chunk store: %v", c.name, err)
		}
	}()
	head, err := ddb.ResolveCommitRef(ctx, ref.NewBranchRef(c.branch))
	if err != nil {
		t.Fatalf("resolve %s's branch %s: %v", c.name, c.branch, err)
	}
	read(ctx, ddb, head)
}

// remoteBytes is the remote's refs/dolt/data tree at one moment, split into
// the current state it holds and the history it holds beyond that state.
// Every write adds rows to the issue event log, so the state grows with the
// writes; the history is what the bound keeps from growing with them.
type remoteBytes struct {
	when          string
	remote, state int64
}

func (b remoteBytes) history() int64 { return b.remote - b.state }

func (b remoteBytes) String() string {
	return fmt.Sprintf("%d bytes of history %s (%d in its tree, %d of current state)", b.history(), b.when, b.remote, b.state)
}

// historyBytes measures the remote's tree against its current state, read
// through c.
func historyBytes(t *testing.T, remote string, c *boundCheckout, when string) remoteBytes {
	t.Helper()
	return remoteBytes{when: when, remote: remoteDataBytes(t, remote), state: c.remoteStateBytes(t)}
}

// remoteDataBytes is the bytes of the table files in the tree the remote's
// refs/dolt/data names, read with git, the manifest left out. A table file
// larger than a blob is a directory of parts, and its parts are summed.
func remoteDataBytes(t *testing.T, remote string) int64 {
	t.Helper()
	cmd := exec.Command("git", "--git-dir", remote, "ls-tree", "-r", "--long", "refs/dolt/data")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	tree, err := cmd.Output()
	if err != nil {
		t.Fatalf("list the remote's refs/dolt/data tree: %v\n%s", err, stderr.String())
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
