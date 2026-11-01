package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/rank"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

const seedStamp = "2026-10-31T00:00:00Z"

// seedInsert writes rows straight into table in chunks, so a test can stand on a
// store of thousands of rows for a second's work where filing them takes minutes.
func seedInsert(t *testing.T, ctx context.Context, st *Store, head string, values []string) {
	t.Helper()
	for chunk := range slices.Chunk(values, 500) {
		if _, err := st.db.ExecContext(ctx, head+strings.Join(chunk, ",")); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func seedCommit(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	for _, statement := range []string{`CALL DOLT_ADD('-A')`, `CALL DOLT_COMMIT('-m','seed')`} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

const (
	seedIssuesHead    = `INSERT INTO issues(id,title,description,status,priority,issue_type,topic,assignee,item_rank,lane,created_at,updated_at) VALUES `
	seedRelationsHead = `INSERT INTO relations(src_id,dst_id,type,created_at,created_by) VALUES `
)

func seedEpicRow(id, key string) string {
	return fmt.Sprintf(`('%s','e','d',NULL,0,'epic','env','','%s','','%s','%s')`, id, key, seedStamp, seedStamp)
}

func seedTaskRow(id, key string) string {
	return fmt.Sprintf(`('%s','t','d','open',0,'task','env','','%s','','%s','%s')`, id, key, seedStamp, seedStamp)
}

func seedEdge(src, dst string, relation model.RelationType) string {
	return fmt.Sprintf(`('%s','%s','%s','%s','seed')`, src, dst, relation, seedStamp)
}

func seededRow(i int) string {
	if i%10 == 0 {
		return fmt.Sprintf("test-e%06d", i)
	}
	return fmt.Sprintf("test-t%06d", i)
}

// seedRows adds rows from..to of perfbench's shape, an epic and its nine tasks,
// and, with blockers, its blocked_by edges.
func seedRows(t *testing.T, ctx context.Context, st *Store, from, to int, blockers bool) {
	t.Helper()
	var issues, edges []string
	for i := from; i < to; i++ {
		key := fmt.Sprintf("K%06d", i)
		if i%10 == 0 {
			issues = append(issues, seedEpicRow(seededRow(i), key))
			continue
		}
		issues = append(issues, seedTaskRow(seededRow(i), key))
		edges = append(edges, seedEdge(seededRow(i), seededRow(i/10*10), model.RelParentChild))
		if blockers && i >= 10 && i%3 == 0 {
			edges = append(edges, seedEdge(seededRow(i), seededRow(i-10), model.RelBlockedBy))
		}
	}
	seedInsert(t, ctx, st, seedIssuesHead, issues)
	seedInsert(t, ctx, st, seedRelationsHead, edges)
	seedCommit(t, ctx, st)
}

// seedKeys adds a top-level epic holding each key, returning their ids in key
// order.
func seedKeys(t *testing.T, ctx context.Context, st *Store, keys []string) []string {
	t.Helper()
	ids := make([]string, len(keys))
	rows := make([]string, len(keys))
	for i, key := range keys {
		ids[i] = fmt.Sprintf("test-k%06d", i)
		rows[i] = seedEpicRow(ids[i], key)
	}
	seedInsert(t, ctx, st, seedIssuesHead, rows)
	seedCommit(t, ctx, st)
	return ids
}

func liveKeys(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../rank/testdata/live_keys.txt")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func storedKey(t *testing.T, ctx context.Context, st *Store, id string) string {
	t.Helper()
	var key string
	if err := st.db.QueryRowContext(ctx, `SELECT item_rank FROM issues WHERE id = ?`, id).Scan(&key); err != nil {
		t.Fatalf("stored key of %s: %v", id, err)
	}
	return key
}

// rankWatch is a RankObserver that remembers what it heard.
type rankWatch struct {
	wrote    []int
	respaces []Respace
}

func (w *rankWatch) on(ctx context.Context) context.Context {
	return WithRankObserver(ctx, RankObserver{
		Wrote:           func(longestKey int) { w.wrote = append(w.wrote, longestKey) },
		Respaced:        func(r Respace) { w.respaces = append(w.respaces, r) },
		ReadParentEdges: func(int) {},
	})
}

func (w *rankWatch) worstRespace() int {
	worst := 0
	for _, r := range w.respaces {
		worst = max(worst, r.Rows)
	}
	return worst
}

// fastest is the least of five runs of read. Load only ever adds time, so a
// minimum is what a busy machine leaves readable.
func fastest(t *testing.T, read func() error) time.Duration {
	t.Helper()
	best := time.Hour
	for range 5 {
		began := time.Now()
		if err := read(); err != nil {
			t.Fatal(err)
		}
		best = min(best, time.Since(began))
	}
	return best
}

func planOf(t *testing.T, ctx context.Context, st *Store, query string, args ...any) string {
	t.Helper()
	rows, err := st.db.QueryContext(ctx, "EXPLAIN PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN PLAN %s: %v", query, err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}

// The premise of rank.MaxLength. Dolt streams a rank order from
// idx_issues_rank(191) with no sort, so keys tied on those chars come back wrong
// with no error. If this fails a Dolt upgrade fixed that, and MaxLength and the
// doctor's warning have no reason left to exist.
func TestKeysTiedPastTheIndexPrefixReadWrong(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	common := strings.Repeat("A", 200)
	order := rand.New(rand.NewSource(1)).Perm(300)
	keys := make([]string, len(order))
	for i, at := range order {
		keys[i] = fmt.Sprintf("%s%05d", common, at+1)
	}
	seedKeys(t, ctx, st, keys)
	slices.Sort(keys)
	var last string
	if err := st.db.QueryRowContext(ctx, `SELECT item_rank FROM issues WHERE item_rank != '' ORDER BY item_rank DESC LIMIT 1`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last == keys[len(keys)-1] {
		t.Fatal("a streamed rank order read tied keys right: Dolt no longer ties on the index prefix, so rank.MaxLength can go")
	}
}

// What MaxLength promises: at the limit no two keys tie, a key that is a prefix
// of a longer legacy key included, so every streamed read is right.
func TestKeysAtMaxLengthReadRight(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	common := strings.Repeat("A", rank.MaxLength-5)
	legacy := strings.Repeat("B", rank.MaxLength)
	var keys []string
	for i := range 300 {
		keys = append(keys, fmt.Sprintf("%s%05d", common, i*7+1))
	}
	keys = append(keys, legacy, legacy+"1", legacy+strings.Repeat("V", 400))
	slices.Sort(keys)
	seedKeys(t, ctx, st, keys)
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 1; i < len(keys)-1; i++ {
		below, err := rankRows(ctx, tx, belowRankQuery, keys[i], 1)
		if err != nil {
			t.Fatal(err)
		}
		above, err := rankRows(ctx, tx, aboveRankQuery, keys[i], 1)
		if err != nil {
			t.Fatal(err)
		}
		if below[0].rank != keys[i-1] || above[0].rank != keys[i+1] {
			t.Fatalf("key %d of %d: neighbours read %.200q and %.200q, want %.200q and %.200q", i, len(keys), below[0].rank, above[0].rank, keys[i-1], keys[i+1])
		}
	}
	for _, c := range []struct {
		edge rankEdge
		want string
	}{{topEdge, keys[0]}, {bottomEdge, keys[len(keys)-1]}} {
		if got, err := workspaceEdgeRankTx(ctx, tx, storage.TopLevel, c.edge); err != nil || got != c.want {
			t.Errorf("the workspace's %s edge read %.200q, %v; want %.200q", c.edge.name, got, err, c.want)
		}
	}
}

func TestMaxLengthIsOneShortOfTheIndexPrefix(t *testing.T) {
	schema, err := os.ReadFile("schema_snapshot.sql")
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile("KEY `idx_issues_rank` \\(`item_rank`\\((\\d+)\\)\\)").FindSubmatch(schema)
	if found == nil {
		t.Fatal("schema_snapshot.sql has no idx_issues_rank over a prefix of item_rank: a key's limit is derived from that prefix")
	}
	prefix, err := strconv.Atoi(string(found[1]))
	if err != nil {
		t.Fatal(err)
	}
	if rank.MaxLength+1 != prefix {
		t.Fatalf("rank.MaxLength = %d and idx_issues_rank's prefix is %d: a key must be shorter than the prefix", rank.MaxLength, prefix)
	}
}

// Every statement a respace streams has to stay a stream: a TopN or a Sort there
// reads every row below the trigger, which is the ticket's bug.
func TestRankReadsPlanAsAnIndexWalk(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	seedRows(t, ctx, st, 0, 60, false)
	for _, c := range []struct {
		name, query string
		args        []any
	}{
		{"below", belowRankQuery, []any{"K000030", 16}},
		{"above", aboveRankQuery, []any{"K000030", 16}},
		{"at", atRankQuery, []any{"K000030"}},
		{"outside the top", topEdge.outsideQuery(), []any{"K000030"}},
		{"outside the bottom", bottomEdge.outsideQuery(), []any{"K000030"}},
	} {
		plan := planOf(t, ctx, st, c.query, c.args...)
		if !strings.Contains(plan, "IndexedTableAccess") || strings.Contains(plan, "TopN") || strings.Contains(plan, "Sort") {
			t.Errorf("%s plans as\n%s\nwant an index walk with no TopN or Sort", c.name, plan)
		}
	}
}

// A merge join reads both tables whole and looks fine on a small store, so these
// are pinned by their plans. frameColumn's subquery prints as text, so it has a
// ratio test instead.
func TestGraphReadsPlanAsLookups(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	seedRows(t, ctx, st, 0, 60, true)
	frame, frameArgs := frameRowsQuery(storage.Frame(seededRow(0)))
	climb, climbArg := rankClimbQuery([]string{seededRow(23), seededRow(53)})
	children, childrenArg := childIDsQuery([]string{seededRow(0)})
	for _, c := range []struct {
		name, query string
		args        []any
	}{
		{"a container frame's rows", frame + ` ORDER BY issues.item_rank ASC LIMIT 1`, frameArgs},
		{"a cycle check's reach", blocksPrecedesQuery, []any{seededRow(23), seededRow(53)}},
		{"a walk's parents", parentEdgesQuery("r.src_id = ?"), []any{seededRow(23)}},
		{"a climb's epics", parentEdgesQuery("r.src_id = ? AND " + epicParentPredicate), []any{seededRow(23)}},
		{"a level of tree order's climb", climb, []any{climbArg}},
		{"a parent's children", children, []any{childrenArg, string(model.RelParentChild)}},
	} {
		plan := planOf(t, ctx, st, c.query, c.args...)
		if !strings.Contains(plan, "LookupJoin") || strings.Contains(plan, "MergeJoin") {
			t.Errorf("%s plans as\n%s\nwant a LookupJoin and no MergeJoin", c.name, plan)
		}
	}
}

// One issue's edges read from each endpoint as a point range on the index's
// leading column. Anything wider there is a walk of the table.
func TestOneIssuesEdgesPlanAsPointLookups(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	seedRows(t, ctx, st, 0, 60, true)
	id := seededRow(23)
	for _, from := range []string{relationsBySrc, relationsByDst} {
		plan := planOf(t, ctx, st, `SELECT src_id, dst_id, type, created_at, created_by FROM `+from, id)
		if !strings.Contains(plan, "filters: [{["+id+", "+id+"]") {
			t.Errorf("%s plans as\n%s\nwant a point range on the leading column", from, plan)
		}
	}
}

// frameColumn's speed lives in its spelling and no plan shows it, so a rewrite or
// a Dolt upgrade that loses it is caught here: linear is 4x and a merge join on
// every row 16x. The top level's edge read walks the index past child rows, and
// that walk has its own ratio, so its cost has a check and not only its place.
func TestAParentRowThatIsGoneFramesNothing(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	seedRows(t, ctx, st, 0, 1, false)
	seedInsert(t, ctx, st, seedIssuesHead, []string{seedTaskRow("test-orphan", "K1")})
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, statement := range []string{`SET FOREIGN_KEY_CHECKS = 0`, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES ` + seedEdge("test-orphan", "test-ghost", model.RelParentChild), `SET FOREIGN_KEY_CHECKS = 1`} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	var frame string
	if err := conn.QueryRowContext(ctx, `SELECT `+frameColumn+` FROM issues WHERE id = 'test-orphan'`).Scan(&frame); err != nil {
		t.Fatal(err)
	}
	if frame != string(storage.TopLevel) {
		t.Errorf("framed by %q, want the top level ancestorChain reads", frame)
	}
}

func TestFrameColumnStaysLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("a timing ratio over two seeded stores")
	}
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	rows, args := frameRowsQuery(storage.TopLevel)
	slots := func() time.Duration {
		return fastest(t, func() error {
			_, err := rankRows(ctx, st.db, rows+` ORDER BY issues.item_rank ASC, issues.id ASC`, args...)
			return err
		})
	}
	seedRows(t, ctx, st, 0, 1000, false)
	small := slots()
	seedRows(t, ctx, st, 1000, 4000, false)
	if large := slots(); large > 8*small {
		t.Errorf("the top-level slot read took %v at 1,000 rows and %v at 4,000, past 8x: frameColumn has stopped being linear", small, large)
	}

	edge := func() time.Duration {
		return fastest(t, func() error {
			var id, key string
			return st.db.QueryRowContext(ctx, rows+` ORDER BY issues.item_rank DESC LIMIT 1`, args...).Scan(&id, &key)
		})
	}
	past := func(from, to int) {
		var issues, edges []string
		for i := from; i < to; i++ {
			id := fmt.Sprintf("test-z%06d", i)
			issues = append(issues, seedTaskRow(id, fmt.Sprintf("Z%06d", i)))
			edges = append(edges, seedEdge(id, seededRow(0), model.RelParentChild))
		}
		seedInsert(t, ctx, st, seedIssuesHead, issues)
		seedInsert(t, ctx, st, seedRelationsHead, edges)
		seedCommit(t, ctx, st)
	}
	past(0, 1000)
	near := edge()
	past(1000, 4000)
	if far := edge(); far > 8*near {
		t.Errorf("the top-level edge read took %v past 1,000 child rows and %v past 4,000, past 8x", near, far)
	}
}

// Cause and check are one read each: a cycle check costs what its walk reaches,
// so a store four times the size costs it nothing, and any one whole-graph read
// put back reads 2x or more. A perfbench probe could not hold this: the old read
// was 60ms at 10,740 rows, far under a one-second budget.
func TestCycleChecksStayLocal(t *testing.T) {
	if testing.Short() {
		t.Skip("a timing ratio over two seeded stores")
	}
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	check := func(rel model.Relation) time.Duration {
		return fastest(t, func() error {
			tx, err := st.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			return rejectCycleTx(ctx, tx, rel)
		})
	}
	edges := func(rows int) (blocked, parent model.Relation) {
		base := rows / 2 / 10 * 10
		return model.Relation{SrcID: seededRow(base + 4), DstID: seededRow(base - 195), Type: model.RelBlockedBy},
			model.Relation{SrcID: seededRow(base + 7), DstID: seededRow(base - 200), Type: model.RelParentChild}
	}
	seedRows(t, ctx, st, 0, 1000, true)
	blocked, parent := edges(1000)
	smallBlocked, smallParent := check(blocked), check(parent)
	seedRows(t, ctx, st, 1000, 4000, true)
	blocked, parent = edges(4000)
	largeBlocked, largeParent := check(blocked), check(parent)
	if float64(largeBlocked) > 1.5*float64(smallBlocked) {
		t.Errorf("a blocked-by check took %v at 1,000 rows and %v at 4,000, past 1.5x: it reads the graph again", smallBlocked, largeBlocked)
	}
	if float64(largeParent) > 1.5*float64(smallParent) {
		t.Errorf("a parent check took %v at 1,000 rows and %v at 4,000, past 1.5x: it reads the graph again", smallParent, largeParent)
	}
}

// A foundation ticket reaches everything waiting on it, and the second every
// command gets holds that. A loop restored data holds ends the read.
func TestACycleCheckReadsWhatItReachesInOneRead(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	const chain = 2000
	seedRows(t, ctx, st, 0, chain*5+10, false)
	link := func(i int) string { return seededRow(i*5 + 1) }
	edges := []string{seedEdge(link(chain+1), link(chain), model.RelBlockedBy)}
	for i := 1; i < chain; i++ {
		edges = append(edges, seedEdge(link(i), link(i-1), model.RelBlockedBy))
	}
	seedInsert(t, ctx, st, seedRelationsHead, edges)
	seedCommit(t, ctx, st)
	check := func(dependent, dependency string) (error, time.Duration) {
		tx, err := st.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		began := time.Now()
		err = rejectBlocksCycle(ctx, tx, dependent, dependency)
		return err, time.Since(began)
	}
	err, took := check(link(0), link(chain-1))
	var refused model.ValidationError
	if !errors.As(err, &refused) {
		t.Fatalf("closing a %d-link chain: %v, want a refusal", chain, err)
	}
	if took > time.Second {
		t.Errorf("a cycle check over %d links took %v, past a second", chain, took)
	}
	if err, _ := check(link(chain-1), link(0)); err != nil {
		t.Errorf("an edge along the chain: %v", err)
	}
	seedInsert(t, ctx, st, seedRelationsHead, []string{seedEdge(link(0), link(chain-1), model.RelBlockedBy)})
	if err, _ := check(link(5), link(chain+1)); err != nil {
		t.Errorf("an edge beside a loop: %v", err)
	}
}

// This store's own keys, on Dolt: every placement stays local, and a burst at one
// spot stays under the cap.
func TestALiveStoreRespacesLocally(t *testing.T) {
	if testing.Short() {
		t.Skip("hundreds of real writes")
	}
	base := context.Background()
	st := openIssueStore(t, base)
	ids := seedKeys(t, base, st, liveKeys(t))
	watch := &rankWatch{}
	ctx := watch.on(base)
	create := func(title string, placement storage.RankPlacement) model.Issue {
		t.Helper()
		issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "env", IssueType: "epic", Placement: placement})
		if err != nil {
			t.Fatal(err)
		}
		return issue
	}
	for i := range 150 {
		create(fmt.Sprintf("appended %d", i), storage.RankBottom)
		create(fmt.Sprintf("prepended %d", i), storage.RankTop)
		interior := create(fmt.Sprintf("interior %d", i), storage.RankBottom)
		if _, err := st.RankAbove(ctx, interior.ID, ids[300+i*5]); err != nil {
			t.Fatal(err)
		}
	}
	if worst := watch.worstRespace(); worst > 64 {
		t.Errorf("worst respace %d rows, want at most 64", worst)
	}
	for i := range 300 {
		issue := create(fmt.Sprintf("burst %d", i), storage.RankBottom)
		if _, err := st.RankAbove(ctx, issue.ID, ids[625]); err != nil {
			t.Fatal(err)
		}
	}
	if worst := watch.worstRespace(); worst > rank.MaxRespace {
		t.Errorf("worst respace %d rows, want at most %d", worst, rank.MaxRespace)
	}
}

// A --top create leads its frame at any epic, not only the first, and says so
// with the key it holds once the respace it set off is done.
func TestATopCreateLeadsItsFrameOnAHotSpot(t *testing.T) {
	if testing.Short() {
		t.Skip("hundreds of real writes")
	}
	base := context.Background()
	st := openIssueStore(t, base)
	ids := seedKeys(t, base, st, liveKeys(t))
	for _, epic := range []string{ids[0], ids[625]} {
		for i := range 300 {
			child, err := st.CreateIssue(base, storage.CreateIssueInput{Prefix: "test", Title: fmt.Sprintf("hot %d", i), Topic: "env", IssueType: "task", ParentID: epic, Placement: storage.RankTop})
			if err != nil {
				t.Fatal(err)
			}
			if i%10 != 0 {
				continue
			}
			var above int
			key := storedKey(t, base, st, child.ID)
			if child.Rank != key {
				t.Fatalf("create %d returned %q, and the row holds %q", i, child.Rank, key)
			}
			if err := st.db.QueryRowContext(base, `SELECT COUNT(*) FROM issues i JOIN relations r ON r.src_id = i.id
				WHERE r.dst_id = ? AND r.type = 'parent-child' AND i.deleted_at IS NULL AND i.item_rank != '' AND i.item_rank < ?`, epic, key).Scan(&above); err != nil {
				t.Fatal(err)
			}
			if above != 0 {
				t.Fatalf("create %d under %s: %d siblings sort above a --top create", i, epic, above)
			}
		}
	}
}

// The top level has no edges, so its bottom is found by walking past child rows.
func TestARankToTheBottomPastChildRowsLeadsTheTopLevelsEnd(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	seedRows(t, ctx, st, 0, 60, false)
	var issues, edges []string
	for i := range 30 {
		id := fmt.Sprintf("test-z%06d", i)
		issues = append(issues, seedTaskRow(id, fmt.Sprintf("Z%06d", i)))
		edges = append(edges, seedEdge(id, seededRow(0), model.RelParentChild))
	}
	seedInsert(t, ctx, st, seedIssuesHead, issues)
	seedInsert(t, ctx, st, seedRelationsHead, edges)
	seedCommit(t, ctx, st)

	moved := seededRow(20)
	if _, err := st.RankToBottom(ctx, moved); err != nil {
		t.Fatal(err)
	}
	key := storedKey(t, ctx, st, moved)
	for _, epic := range []string{seededRow(0), seededRow(10), seededRow(30), seededRow(50)} {
		if other := storedKey(t, ctx, st, epic); other >= key {
			t.Errorf("%s holds %q, at or past the %q of the epic sent to the bottom", epic, other, key)
		}
	}
}

// One answer to who is in a frame: the edge read, rank set and the frame lookup
// agree about a child restored data gives two parents, so moving the top of its
// true frame to the top moves nothing and says so.
func TestAChildTwoParentsClaimSitsInOneFrame(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	first := parentChain(t, ctx, st, "E1", "C")
	second := parentChain(t, ctx, st, "E2", "D")
	child := first[1]
	seedParentEdge(t, ctx, st, child.ID, second[0].ID)
	lower, upper := min(first[0].ID, second[0].ID), max(first[0].ID, second[0].ID)
	sibling := mustCreate(t, st, storage.CreateIssueInput{Prefix: "test", Title: "beside it", Topic: "env", IssueType: "task", ParentID: lower})

	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := frameOfTx(ctx, tx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f != storage.Frame(lower) {
		t.Errorf("frameOfTx(%s) = %q, want the lower parent %q", child.ID, f, lower)
	}
	for parent, wants := range map[string]bool{lower: true, upper: false} {
		rows, args := frameRowsQuery(storage.Frame(parent))
		slots, err := rankRows(ctx, tx, rows, args...)
		if err != nil {
			t.Fatal(err)
		}
		in := slices.ContainsFunc(slots, func(r rankedIssue) bool { return r.id == child.ID })
		if in != wants {
			t.Errorf("%s in the frame of %s = %t, want %t", child.ID, parent, in, wants)
		}
	}
	_ = tx.Rollback()

	onTop, err := st.RankToTop(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.RankToTop(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if onTop.Frame != storage.Frame(lower) || again.Moved {
		t.Errorf("moving the top of its frame to the top = %+v after %+v, want no move in %q", again, onTop, lower)
	}
	set, err := st.RankSet(ctx, []string{sibling.ID, child.ID})
	if err != nil {
		t.Fatal(err)
	}
	if set.Frame != storage.Frame(lower) {
		t.Errorf("RankSet frame = %q, want %q", set.Frame, lower)
	}
}

// The ticket's bug, at the ten-times store: bottom appends do no respace at all,
// from keys the rule itself wrote.
func TestBottomAppendsDoNoRespaceAtTenTimesTheStore(t *testing.T) {
	if testing.Short() {
		t.Skip("a 10,540-row seed")
	}
	base := context.Background()
	st := openIssueStore(t, base)
	keys := make([]string, 10540)
	last := ""
	for i := range keys {
		key, err := rank.Midpoint(last, "")
		if err != nil {
			t.Fatal(err)
		}
		keys[i], last = key, key
	}
	seedKeys(t, base, st, keys)
	watch := &rankWatch{}
	ctx := watch.on(base)
	for i := range 200 {
		if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: fmt.Sprintf("row %d", i), Topic: "env", IssueType: "epic"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(watch.respaces) != 0 {
		t.Fatalf("%d respaces over 200 bottom appends at 10,740 rows, want none: %+v", len(watch.respaces), watch.respaces[0])
	}
}

func TestDoctorWarnsOnTiedKeysAndNotOnLongOnes(t *testing.T) {
	for _, c := range []struct {
		name string
		keys []string
		want bool
	}{
		{"legacy long keys", []string{strings.Repeat("A", 300), strings.Repeat("B", 648)}, false},
		{"long keys tied on the prefix", []string{strings.Repeat("A", 200) + "1", strings.Repeat("A", 200) + "2"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			st := openIssueStore(t, ctx)
			seedKeys(t, ctx, st, c.keys)
			report, err := st.Doctor(ctx)
			if err != nil {
				t.Fatal(err)
			}
			warned := slices.ContainsFunc(report.Warnings, func(w string) bool { return strings.Contains(w, "tied on the index prefix: 1") })
			if warned != c.want {
				t.Errorf("Doctor().Warnings = %v, want a tied-key warning: %t", report.Warnings, c.want)
			}
		})
	}
}

// A sync landing cannot fail for want of room, which rank_collisions.go promises:
// a gap with no room inside MaxLength widens, as a respace does, and two clones
// landing one export must write the same rows.
// A closed bound that is the row a move vacates opens that end for the next
// placement, so a respace in a corner leaves a step past it, and the move lands.
func TestAMoveOutOfADeepCornerFindsRoom(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	corner := strings.Repeat("z", 140)
	ids := seedKeys(t, ctx, st, []string{corner + "1", corner + "2", corner + "3"})
	if _, err := st.RankBelow(ctx, ids[2], ids[1]); err != nil {
		t.Fatalf("RankBelow out of the corner: %v", err)
	}
	keys := map[string]string{}
	rows, err := rankRows(ctx, st.db, `SELECT id, item_rank FROM issues WHERE deleted_at IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		keys[row.id] = row.rank
	}
	if !(keys[ids[0]] < keys[ids[1]] && keys[ids[1]] < keys[ids[2]]) {
		t.Fatalf("order after the move: %v, want %v", keys, ids)
	}
}

// The doctor's warning names this repair: keys tied on the index prefix come
// out told apart, in the order they stood, on a store whose keys read right.
func TestARepairSeparatesKeysTiedOnTheIndexPrefix(t *testing.T) {
	ctx := context.Background()
	st := openIssueStore(t, ctx)
	common := strings.Repeat("A", 200)
	var keys []string
	for i := 1; i <= 40; i++ {
		keys = append(keys, fmt.Sprintf("%s%02d", common, i))
	}
	ids := seedKeys(t, ctx, st, slices.Concat([]string{"1"}, keys, []string{"z"}))
	if _, err := st.FixRankInversions(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := loadRankOrder(ctx, st.db)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(stored, compareRankedIssues)
	got := make([]string, len(stored))
	repaired := make([]string, len(stored))
	for i, row := range stored {
		got[i], repaired[i] = row.id, row.rank
	}
	if !slices.Equal(got, ids) {
		t.Fatalf("repair changed the order: %v, want %v", got, ids)
	}
	if tied := rank.PrefixTied(repaired); tied != 0 {
		t.Fatalf("%d keys still tied on the prefix after the repair", tied)
	}
}

// A repair's movers can all fall in one spot, whose first respaces replace the
// keys the later triggers name. Each respace costs a window, so those must not
// run one each: 60 rows take a few windows, not 59.
func TestARepairDoesNotRespaceKeysAlreadyReplaced(t *testing.T) {
	base := context.Background()
	st := openIssueStore(t, base)
	shared := strings.Repeat("A", 60)
	var keys []string
	keys = append(keys, "1", "2", shared+"1")
	for range 60 {
		keys = append(keys, shared+"5")
	}
	keys = append(keys, shared+"9", "y", "z")
	seedKeys(t, base, st, keys)
	watch := &rankWatch{}
	if _, err := st.FixRankInversions(watch.on(base)); err != nil {
		t.Fatal(err)
	}
	if len(watch.respaces) == 0 || len(watch.respaces) > 8 {
		t.Fatalf("a repair of one spot ran %d respaces, want a few windows' worth", len(watch.respaces))
	}
}

func TestRankRewritesWidenPastAGapWithNoRoom(t *testing.T) {
	shared := strings.Repeat("x", 188)
	sequence := []placement{
		{rankedIssue: rankedIssue{id: "a", rank: "Aa"}, anchored: true},
		{rankedIssue: rankedIssue{id: "b", rank: shared + "11"}, anchored: true},
		{rankedIssue: rankedIssue{id: "m1", rank: "zz"}},
		{rankedIssue: rankedIssue{id: "m2", rank: "zz"}},
		{rankedIssue: rankedIssue{id: "c", rank: shared + "12"}, anchored: true},
		{rankedIssue: rankedIssue{id: "d", rank: "zzzz"}, anchored: true},
	}
	rewrites, err := rankRewrites(sequence)
	if err != nil {
		t.Fatalf("rankRewrites with no room between anchors: %v", err)
	}
	again, err := rankRewrites(sequence)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rewrites, again) {
		t.Fatalf("rankRewrites is not deterministic: %v then %v", rewrites, again)
	}
	written := map[string]string{}
	for _, r := range rewrites {
		written[r.id] = r.newRank
		if len(r.newRank) > rank.MaxLength {
			t.Fatalf("%s rewritten to %d chars", r.id, len(r.newRank))
		}
		if mover := strings.HasPrefix(r.id, "m"); r.respaced == mover {
			t.Errorf("%s respaced: %t, want the bound the gap gave up to be the one respaced", r.id, r.respaced)
		}
	}
	previous := ""
	for _, p := range sequence {
		key := p.rank
		if rewritten, ok := written[p.id]; ok {
			key = rewritten
		}
		if key <= previous {
			t.Fatalf("%s at %.40q does not sort past %.40q", p.id, key, previous)
		}
		previous = key
	}
	if len(rewrites) != 3 {
		t.Errorf("%d rewrites, want the two movers and the one anchor the run widened past", len(rewrites))
	}
}

// keysAround is count keys of the given length, one unit apart, inside a store
// of short keys on each side, with the row at the middle to move something
// between.
func keysAround(t *testing.T, length, count int) []string {
	t.Helper()
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	keys := []string{"A"}
	for i := range count {
		keys = append(keys, fmt.Sprintf("%s%c%c", strings.Repeat("V", length-2), alphabet[i/len(alphabet)], alphabet[i%len(alphabet)]))
	}
	return append(keys, "b")
}

// A respace, and what the event says about it, in each shape the design names.
func TestTheObserverHearsEachRespace(t *testing.T) {
	for _, c := range []struct {
		name        string
		keys        []string
		wantRows    func(int) bool
		wantLongest func(longest, ceiling int) bool
	}{
		{"local: a crowded key takes its window", keysAround(t, 7, 40),
			func(rows int) bool { return rows > 0 && rows <= 2*rank.SmoothingWindow },
			func(longest, ceiling int) bool { return longest < ceiling }},
		{"capped: length absorbs what the window does not", keysAround(t, 7, 600),
			func(rows int) bool { return rows > 32 && rows <= rank.MaxRespace },
			func(longest, ceiling int) bool { return longest >= ceiling }},
		{"past the cap: no room inside MaxLength", keysAround(t, rank.MaxLength, 400),
			func(rows int) bool { return rows > rank.MaxRespace },
			func(int, int) bool { return true }},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := context.Background()
			st := openIssueStore(t, base)
			ids := seedKeys(t, base, st, c.keys)
			watch := &rankWatch{}
			if _, err := st.RankAbove(watch.on(base), ids[len(ids)-1], ids[len(ids)/2]); err != nil {
				t.Fatal(err)
			}
			if len(watch.respaces) == 0 {
				t.Fatalf("no respace heard, wrote %v", watch.wrote)
			}
			respace := watch.respaces[0]
			if !c.wantRows(respace.Rows) || !c.wantLongest(respace.LongestKey, respace.Ceiling) {
				t.Errorf("respace = %+v", respace)
			}
			if respace.Rewritten == 0 || respace.Rewritten > respace.Rows {
				t.Errorf("respace rewrote %d of %d rows", respace.Rewritten, respace.Rows)
			}
			if len(watch.wrote) == 0 {
				t.Error("a move that wrote keys reported no longest key")
			}
		})
	}
}

func TestAPlacementThatDoesNotRespaceSaysWhatItWrote(t *testing.T) {
	t.Run("in open keyspace", func(t *testing.T) {
		base := context.Background()
		st := openIssueStore(t, base)
		watch := &rankWatch{}
		ctx := watch.on(base)
		for i := range 2 {
			if _, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: fmt.Sprintf("row %d", i), Topic: "env", IssueType: "epic"}); err != nil {
				t.Fatal(err)
			}
		}
		if len(watch.respaces) != 0 || len(watch.wrote) == 0 || slices.Max(watch.wrote) > 4 {
			t.Errorf("heard respaces %+v and wrote %v, want none and short keys", watch.respaces, watch.wrote)
		}
	})
	// A key at its ceiling beside neighbours as long says nothing about density,
	// and the price it paid in length is still reported.
	t.Run("past its ceiling", func(t *testing.T) {
		base := context.Background()
		st := openIssueStore(t, base)
		ids := seedKeys(t, base, st, []string{"A", "V0000000", "V000000A", "V000000K", "b"})
		watch := &rankWatch{}
		if _, err := st.RankAbove(watch.on(base), ids[4], ids[2]); err != nil {
			t.Fatal(err)
		}
		if len(watch.respaces) != 0 {
			t.Errorf("heard respaces %+v, want none", watch.respaces)
		}
		if len(watch.wrote) == 0 || slices.Max(watch.wrote) != 8 {
			t.Errorf("wrote %v, want a key of 8 chars reported", watch.wrote)
		}
	})
}

// A write that did not land is heard by nobody: a key a rolled-back attempt
// respaced was never written, and an event saying so would be false.
func TestAWriteThatDidNotStandIsNotHeard(t *testing.T) {
	refused := errors.New("refused")
	respaceThenRefuse := func(ctx context.Context, tx *sql.Tx) error {
		if err := smoothRanksTx(ctx, tx, "VVV0010"); err != nil {
			return err
		}
		return refused
	}
	t.Run("a rolled-back attempt", func(t *testing.T) {
		base := context.Background()
		st := openIssueStore(t, base)
		seedKeys(t, base, st, keysAround(t, 7, 40))
		watch := &rankWatch{}
		if err := st.withMutation(watch.on(base), "refused", respaceThenRefuse); !errors.Is(err, refused) {
			t.Fatalf("withMutation error = %v", err)
		}
		if len(watch.respaces) != 0 || len(watch.wrote) != 0 {
			t.Errorf("heard respaces %+v and wrote %v from a rolled-back attempt", watch.respaces, watch.wrote)
		}
	})
	t.Run("a refused batch", func(t *testing.T) {
		base := context.Background()
		st := openIssueStore(t, base)
		seedKeys(t, base, st, keysAround(t, 7, 40))
		watch := &rankWatch{}
		_, _, err := withBatch(watch.on(base), st, func(ctx context.Context, _ *storage.BulkApplyResult) error {
			if err := st.withMutation(ctx, "stood alone", func(ctx context.Context, tx *sql.Tx) error {
				return smoothRanksTx(ctx, tx, "VVV0010")
			}); err != nil {
				return err
			}
			return refused
		})
		if !errors.Is(err, refused) {
			t.Fatalf("withBatch error = %v", err)
		}
		if len(watch.respaces) != 0 || len(watch.wrote) != 0 {
			t.Errorf("heard respaces %+v and wrote %v from a refused batch", watch.respaces, watch.wrote)
		}
	})
	t.Run("a write that stands is heard once", func(t *testing.T) {
		base := context.Background()
		st := openIssueStore(t, base)
		seedKeys(t, base, st, keysAround(t, 7, 40))
		watch := &rankWatch{}
		if err := st.withMutation(watch.on(base), "stands", func(ctx context.Context, tx *sql.Tx) error {
			return smoothRanksTx(ctx, tx, "VVV0010")
		}); err != nil {
			t.Fatal(err)
		}
		if len(watch.respaces) != 1 {
			t.Errorf("heard %d respaces, want one", len(watch.respaces))
		}
	})
}
