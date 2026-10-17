// hb: scratch harness for links-storage-3k6u.8ka. Measures what re-rooting
// master onto a parentless commit does to store size, and whether two
// independent rewrites of the same chain produce identical hashes.
// Runs ONLY against copies under /tmp.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
	"github.com/dolthub/dolt/go/store/hash"
	embedded "github.com/dolthub/driver"
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func guard(dir string) {
	abs, _ := filepath.Abs(dir)
	if !strings.HasPrefix(abs, "/tmp/") && !strings.HasPrefix(abs, "/private/tmp/") {
		fmt.Fprintln(os.Stderr, "refusing: not under /tmp:", abs)
		os.Exit(2)
	}
}

// dbDir is the directory holding .dolt (…/.git/links/dolt/links).
func openDDB(ctx context.Context, dbDir string) *doltdb.DoltDB {
	guard(dbDir)
	fs, err := filesys.LocalFS.WithWorkingDir(dbDir)
	must(err)
	dEnv := env.Load(ctx, env.GetCurrentUserHomeDir, fs, doltdb.LocalDirDoltDB, "hb")
	must(dEnv.DBLoadError)
	return dEnv.DoltDB(ctx)
}

type node struct {
	c       *doltdb.Commit
	h       hash.Hash
	height  uint64
	parents []hash.Hash
}

func load(ctx context.Context, ddb *doltdb.DoltDB, h hash.Hash) *node {
	oc, err := ddb.ResolveHash(ctx, h)
	must(err)
	if oc.Commit == nil {
		must(fmt.Errorf("ghost commit %s", h))
	}
	ht, err := oc.Commit.Height()
	must(err)
	ps, err := oc.Commit.ParentHashes(ctx)
	must(err)
	return &node{c: oc.Commit, h: h, height: ht, parents: ps}
}

func ancestors(ctx context.Context, ddb *doltdb.DoltDB, from hash.Hash) map[hash.Hash]bool {
	seen := map[hash.Hash]bool{}
	stack := []hash.Hash{from}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[h] {
			continue
		}
		seen[h] = true
		stack = append(stack, load(ctx, ddb, h).parents...)
	}
	return seen
}

// rewrite re-roots `branch` at the commit `window` first-parent steps below its
// head: a parentless commit carrying the cut commit's root value and meta, then
// every commit after the cut re-parented with its own root value and meta.
func rewrite(ctx context.Context, ddb *doltdb.DoltDB, branch string, window int, cutAt string, apply bool) {
	start := time.Now()
	br := ref.NewBranchRef(branch)
	head, err := ddb.ResolveCommitRef(ctx, br)
	must(err)
	headH, err := head.HashOf()
	must(err)
	cut := load(ctx, ddb, headH)
	if cutAt != "" {
		h, ok := hash.MaybeParse(cutAt)
		if !ok {
			must(fmt.Errorf("bad hash %s", cutAt))
		}
		cut = load(ctx, ddb, h)
	}
	for i := 0; i < window; i++ {
		if len(cut.parents) == 0 {
			must(fmt.Errorf("history shorter than window %d", window))
		}
		cut = load(ctx, ddb, cut.parents[0])
	}
	old := ancestors(ctx, ddb, cut.h) // includes cut
	// kept: reachable from head, not an ancestor-or-self of cut.
	kept := map[hash.Hash]*node{}
	stack := []hash.Hash{headH}
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if old[h] || kept[h] != nil {
			continue
		}
		n := load(ctx, ddb, h)
		kept[h] = n
		stack = append(stack, n.parents...)
	}
	order := make([]*node, 0, len(kept))
	for _, n := range kept {
		order = append(order, n)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].height != order[j].height {
			return order[i].height < order[j].height
		}
		return order[i].h.String() < order[j].h.String()
	})

	newOf := map[hash.Hash]*doltdb.Commit{}
	commitLike := func(n *node, parents []*doltdb.Commit) *doltdb.Commit {
		rv, err := n.c.GetRootValue(ctx)
		must(err)
		rvh, err := rv.HashOf()
		must(err)
		meta, err := n.c.GetCommitMeta(ctx)
		must(err)
		nc, err := ddb.CommitDanglingWithParentCommits(ctx, rvh, parents, meta)
		must(err)
		return nc
	}
	{
		// A parentless commit is only buildable by committing to a ref with no
		// head (datas.BuildNewCommit); the dangling path refuses it.
		rv, err := cut.c.GetRootValue(ctx)
		must(err)
		rvh, err := rv.HashOf()
		must(err)
		meta, err := cut.c.GetCommitMeta(ctx)
		must(err)
		tmp := ref.NewBranchRef("hb-root-tmp")
		root, err := ddb.CommitWithParentCommits(ctx, rvh, tmp, nil, meta)
		must(err)
		newOf[cut.h] = root
		defer func() { must(ddb.DeleteBranch(ctx, tmp, nil)) }()
	}
	detached := 0
	for _, n := range order {
		var ps []*doltdb.Commit
		seenP := map[hash.Hash]bool{}
		for _, p := range n.parents {
			np, ok := newOf[p]
			if !ok {
				continue // parent older than the cut: dropped
			}
			nh, _ := np.HashOf()
			if !seenP[nh] {
				seenP[nh] = true
				ps = append(ps, np)
			}
		}
		if len(ps) == 0 { // side line forked below the cut: hang it on the new root
			detached++
			ps = []*doltdb.Commit{newOf[cut.h]}
		}
		newOf[n.h] = commitLike(n, ps)
	}
	newHead := newOf[headH]
	newHeadH, _ := newHead.HashOf()
	rootH, _ := newOf[cut.h].HashOf()
	if apply {
		must(ddb.SetHeadToCommit(ctx, br, newHead))
	}
	fmt.Printf("old_head=%s cut=%s cut_height=%d new_root=%s new_head=%s kept=%d detached=%d applied=%v elapsed=%s\n",
		headH, cut.h, cut.height, rootH, newHeadH, len(kept), detached, apply, time.Since(start).Round(time.Millisecond))
}

func refs(ctx context.Context, ddb *doltdb.DoltDB) {
	rs, err := ddb.GetRefsWithHashes(ctx)
	must(err)
	counts := map[string]int{}
	for _, r := range rs {
		counts[string(r.Ref.GetType())]++
		if r.Ref.GetType() != ref.RemoteRefType || counts[string(r.Ref.GetType())] <= 5 {
			fmt.Printf("%s %s\n", r.Hash, r.Ref.String())
		}
	}
	fmt.Println("counts:", counts)
}

func dropAllBut(ctx context.Context, ddb *doltdb.DoltDB, keep string) {
	root, err := ddb.NomsRoot(ctx)
	must(err)
	dm, err := ddb.DatasetsByRootHash(ctx, root)
	must(err)
	var keys []string
	must(dm.IterAll(ctx, func(k string, _ hash.Hash) error { keys = append(keys, k); return nil }))
	n := 0
	for _, k := range keys {
		if k == "refs/heads/"+keep || k == "workingSets/heads/"+keep {
			continue
		}
		if ref.IsWorkingSet(k) {
			wsr, err := ref.WorkingSetRefForHead(ref.NewBranchRef(strings.TrimPrefix(k, "workingSets/heads/")))
			must(err)
			must(ddb.DeleteWorkingSet(ctx, wsr))
		} else {
			r, err := ref.Parse(k)
			must(err)
			must(ddb.DeleteBranch(ctx, r, nil))
		}
		n++
	}
	fmt.Println("deleted datasets:", n)
}

func sqlDB(doltRoot string) *sql.DB {
	guard(doltRoot)
	c, err := embedded.NewConnector(embedded.Config{Directory: doltRoot, CommitName: "hb", CommitEmail: "hb@links.local", Database: "links", DisableSingletonCache: true})
	must(err)
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(1)
	return db
}

func main() {
	ctx := context.Background()
	cmd, dir := os.Args[1], os.Args[2]
	switch cmd {
	case "refs":
		refs(ctx, openDDB(ctx, dir))
	case "rewrite", "dryrun":
		w, err := strconv.Atoi(os.Args[3])
		must(err)
		rewrite(ctx, openDDB(ctx, dir), "master", w, "", cmd == "rewrite")
	case "rewriteat":
		rewrite(ctx, openDDB(ctx, dir), "master", 0, os.Args[3], true)
	case "datasets":
		ddb := openDDB(ctx, dir)
		root, err := ddb.NomsRoot(ctx)
		must(err)
		dm, err := ddb.DatasetsByRootHash(ctx, root)
		must(err)
		must(dm.IterAll(ctx, func(k string, h hash.Hash) error {
			fmt.Println(h, k)
			return nil
		}))
	case "remotegc": // dir = git+file URL, arg3 = cache root
		start := time.Now()
		ddb, err := doltdb.LoadDoltDBWithParams(ctx, types.Format_Default, dir, filesys.LocalFS,
			map[string]interface{}{dbfactory.GitCacheRootParam: os.Args[3]})
		must(err)
		rs, _ := ddb.GetRefsWithHashes(ctx)
		for _, r := range rs {
			fmt.Println("remote ref", r.Ref.String(), r.Hash.String())
		}
		must(ddb.GC(ctx, types.GCModeFull, chunks.NoArchive, nil))
		must(ddb.Close())
		fmt.Println("remote gc full elapsed", time.Since(start).Round(time.Millisecond))
	case "dropall":
		dropAllBut(ctx, openDDB(ctx, dir), "master")
	case "gc": // dir = dolt root (…/.git/links/dolt)
		db := sqlDB(dir)
		start := time.Now()
		_, err := db.Exec("CALL DOLT_GC('--full')")
		must(err)
		fmt.Println("gc full elapsed", time.Since(start).Round(time.Millisecond))
	case "count":
		db := sqlDB(dir)
		var n int
		must(db.QueryRow("SELECT COUNT(*) FROM dolt_log()").Scan(&n))
		var h string
		must(db.QueryRow("SELECT commit_hash FROM dolt_log() LIMIT 1").Scan(&h))
		fmt.Println("dolt_log commits:", n, "head:", h)
	case "sql":
		db := sqlDB(dir)
		rows, err := db.Query(os.Args[3])
		must(err)
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			must(rows.Scan(ptrs...))
			parts := []string{}
			for _, v := range vals {
				parts = append(parts, v.String)
			}
			fmt.Println(strings.Join(parts, "\t"))
		}
		must(rows.Err())
	default:
		must(fmt.Errorf("unknown cmd %s", cmd))
	}
}
