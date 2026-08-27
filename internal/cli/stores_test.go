package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func gitInit(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init")
}

func addLitStore(t *testing.T, repoDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git", "links", "dolt"), 0o755); err != nil {
		t.Fatalf("addLitStore(%q) error = %v", repoDir, err)
	}
}

// TestRunStoresListsDiscoveredStores drives the command surface: given an
// explicit root over a tree with two lit repos and one lit-less git repo, it
// prints exactly the two store directories, sorted, one per line.
func TestRunStoresListsDiscoveredStores(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	repoA := filepath.Join(root, "repoA")
	if err := os.MkdirAll(repoA, 0o755); err != nil {
		t.Fatalf("mkdir repoA: %v", err)
	}
	gitInit(t, repoA)
	addLitStore(t, repoA)

	repoB := filepath.Join(root, "repoB")
	if err := os.MkdirAll(repoB, 0o755); err != nil {
		t.Fatalf("mkdir repoB: %v", err)
	}
	gitInit(t, repoB)
	addLitStore(t, repoB)

	gitOnly := filepath.Join(root, "gitOnly")
	if err := os.MkdirAll(gitOnly, 0o755); err != nil {
		t.Fatalf("mkdir gitOnly: %v", err)
	}
	gitInit(t, gitOnly)

	var out bytes.Buffer
	if err := runStores(context.Background(), &out, []string{root}); err != nil {
		t.Fatalf("runStores() error = %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("runStores() printed %d lines, want 2:\n%s", len(lines), out.String())
	}
	wantA, err := filepath.EvalSymlinks(filepath.Join(repoA, ".git"))
	if err != nil {
		t.Fatalf("EvalSymlinks(repoA/.git) error = %v", err)
	}
	wantB, err := filepath.EvalSymlinks(filepath.Join(repoB, ".git"))
	if err != nil {
		t.Fatalf("EvalSymlinks(repoB/.git) error = %v", err)
	}
	if lines[0] != filepath.Join(wantA, "links") || lines[1] != filepath.Join(wantB, "links") {
		t.Fatalf("runStores() output = %q, want repoA then repoB store dirs", lines)
	}
}

// TestRunStoresPropagatesDiscoverError pins the error contract: when Discover
// fails, runStores returns the error rather than printing an empty success. A
// non-existent root makes the filesystem walk fail deterministically, without
// coupling the test to git-resolution mechanics. [LAW:behavior-not-structure]
func TestRunStoresPropagatesDiscoverError(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	var out bytes.Buffer
	err := runStores(context.Background(), &out, []string{missing})
	if err == nil {
		t.Fatalf("runStores() returned nil error with output %q; want a surfaced Discover failure", out.String())
	}
	if out.Len() != 0 {
		t.Fatalf("runStores() emitted %q before failing; want no output on the error path", out.String())
	}
}

// TestRunStoresEmptyWhenNoStores confirms a store-less root exits cleanly with
// no output rather than erroring.
func TestRunStoresEmptyWhenNoStores(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "plain"), 0o755); err != nil {
		t.Fatalf("mkdir plain: %v", err)
	}

	var out bytes.Buffer
	if err := runStores(context.Background(), &out, []string{root}); err != nil {
		t.Fatalf("runStores() error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("runStores() output = %q, want empty", out.String())
	}
}

// TestPrintCrossProjectRollupTableAndErrors pins the render contract: readable
// projects become count rows summed into a TOTAL, and an unreadable project
// renders as a marked error row AFTER the table without disturbing the counts —
// the criterion's "error row while the other projects still render".
// [LAW:behavior-not-structure] Asserts the emitted view, not how it was built.
func TestPrintCrossProjectRollupTableAndErrors(t *testing.T) {
	t.Parallel()
	rows := []projectRollup{
		{Label: "alpha", StorageDir: "/repos/alpha/.git/links", Ready: 2, InFlight: 1, Blocked: 3},
		{Label: "/repos/broken/.git/links", StorageDir: "/repos/broken/.git/links", Err: errors.New("open store: manifest missing")},
		{Label: "beta", StorageDir: "/repos/beta/.git/links", Ready: 4, InFlight: 0, Blocked: 1},
	}

	var out bytes.Buffer
	if err := printCrossProjectRollup(&out, rows); err != nil {
		t.Fatalf("printCrossProjectRollup() error = %v", err)
	}
	got := out.String()

	for _, want := range []string{"PROJECT", "READY", "IN-FLIGHT", "BLOCKED"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing header %q:\n%s", want, got)
		}
	}
	// Both readable projects render...
	if !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") {
		t.Fatalf("output missing a readable project row:\n%s", got)
	}
	// ...the TOTAL sums only the readable projects (2+4 ready, 1+0 in-flight, 3+1
	// blocked), the broken one contributing nothing...
	if !strings.Contains(got, "TOTAL") {
		t.Fatalf("output missing TOTAL row:\n%s", got)
	}
	totalLine := lineContaining(t, got, "TOTAL")
	for _, want := range []string{"6", "1", "4"} {
		if !strings.Contains(totalLine, want) {
			t.Fatalf("TOTAL line %q missing aggregate count %q", totalLine, want)
		}
	}
	// ...and the broken store is a loud, marked error row naming its path and cause.
	errLine := lineContaining(t, got, "manifest missing")
	if !strings.HasPrefix(strings.TrimSpace(errLine), "!") {
		t.Fatalf("error row %q is not marked with a leading '!'", errLine)
	}
	if !strings.Contains(errLine, "/repos/broken/.git/links") {
		t.Fatalf("error row %q does not name the unreadable store path", errLine)
	}
}

// TestPrintCrossProjectRollupEmptyIsExplicit pins the empty-input contract: no
// discovered stores prints a plain sentinel, not a header and an all-zeros TOTAL
// that a reader could not tell apart from "every project is empty".
func TestPrintCrossProjectRollupEmptyIsExplicit(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := printCrossProjectRollup(&out, nil); err != nil {
		t.Fatalf("printCrossProjectRollup(nil) error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "no stores discovered") {
		t.Fatalf("empty output %q lacks the '(no stores discovered)' sentinel", got)
	}
	for _, unwanted := range []string{"PROJECT", "TOTAL"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("empty output %q should not render the %q table", got, unwanted)
		}
	}
}

// TestPrintCrossProjectRollupAllErroredSkipsTable pins the all-error contract:
// when every row carries an error, the count table (header + zero TOTAL) is
// omitted — a reader sees only the self-labeled error lines, never a misleading
// "all projects empty" summary above them.
func TestPrintCrossProjectRollupAllErroredSkipsTable(t *testing.T) {
	t.Parallel()
	rows := []projectRollup{
		{StorageDir: "/repos/a/.git/links", Err: errors.New("open store: locked")},
		{StorageDir: "/repos/b/.git/links", Err: errors.New("read config: missing")},
	}
	var out bytes.Buffer
	if err := printCrossProjectRollup(&out, rows); err != nil {
		t.Fatalf("printCrossProjectRollup() error = %v", err)
	}
	got := out.String()
	for _, unwanted := range []string{"PROJECT", "TOTAL"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("all-error output %q should not render the %q table", got, unwanted)
		}
	}
	if strings.Count(got, "!") != 2 {
		t.Fatalf("all-error output %q should carry exactly two marked error lines", got)
	}
}

// TestPrintCrossProjectRollupCloseWarningKeepsCounts pins the intermediate
// state: a store that read cleanly but whose read-only close warned still shows
// its counts in the table AND a distinct `~` close-warning note — the warning
// never suppresses the valid data the way a read error does.
func TestPrintCrossProjectRollupCloseWarningKeepsCounts(t *testing.T) {
	t.Parallel()
	rows := []projectRollup{
		{Label: "alpha", StorageDir: "/repos/alpha/.git/links", Ready: 3, InFlight: 1, Blocked: 0,
			CloseErr: errors.New("engine failed to release lock")},
	}
	var out bytes.Buffer
	if err := printCrossProjectRollup(&out, rows); err != nil {
		t.Fatalf("printCrossProjectRollup() error = %v", err)
	}
	got := out.String()
	// Counts still render in the table (the read succeeded)...
	countLine := lineContaining(t, got, "alpha")
	for _, want := range []string{"3", "1", "0"} {
		if !strings.Contains(countLine, want) {
			t.Fatalf("count line %q missing count %q — a close warning must not suppress counts", countLine, want)
		}
	}
	// ...and the close warning appears as a distinct `~` note, not a `!` error row.
	warnLine := lineContaining(t, got, "close warning")
	if !strings.HasPrefix(strings.TrimSpace(warnLine), "~") {
		t.Fatalf("close-warning line %q is not marked with a leading '~'", warnLine)
	}
	if strings.Contains(got, "! /repos/alpha") {
		t.Fatalf("a close warning must not render as a '!' error row:\n%s", got)
	}
}

// TestGatherCrossProjectRollupUnreadableStoreIsErrorRow drives the gather path:
// a tree of two discovered-but-unopenable stores yields two error rows rather
// than a fatal error, so one broken store never aborts the whole overview.
// [LAW:no-silent-failure] The stores are surfaced as errors, not skipped.
func TestGatherCrossProjectRollupUnreadableStoreIsErrorRow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"repoA", "repoB"} {
		repo := filepath.Join(root, name)
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		gitInit(t, repo)
		addLitStore(t, repo) // a store DIRECTORY exists but holds no real dolt data
	}

	rows, err := gatherCrossProjectRollup(context.Background(), []string{root})
	if err != nil {
		t.Fatalf("gatherCrossProjectRollup() error = %v; want error rows, not a fatal error", err)
	}
	if len(rows) != 2 {
		t.Fatalf("gatherCrossProjectRollup() returned %d rows, want 2:\n%+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.Err == nil {
			t.Fatalf("row %q has nil Err; an empty store dir must not open cleanly", row.StorageDir)
		}
	}
}

// seedDiscoverableStore builds a real, Discover-able lit store under repoDir's
// git-common-dir (`.git/links`) with a known backlog: two ready leaves, one
// in-progress leaf, and one leaf blocked by an unfinished dependency. It opens
// the store read-write only to seed, then CLOSES it — so a later read-only open
// (as the rollup does) is not a concurrent second engine on the same path. It
// returns the storage directory and the issue prefix it stamped into config.json,
// the label the rollup should surface for this project.
func seedDiscoverableStore(t *testing.T, repoDir, prefix, workspaceID string) (storageDir, wantPrefix string) {
	t.Helper()
	ctx := context.Background()
	storageDir = filepath.Join(repoDir, ".git", "links")
	dbPath := filepath.Join(storageDir, "dolt")

	st, err := engine.Open(ctx, engine.ReadWrite, dbPath, workspaceID)
	if err != nil {
		t.Fatalf("engine.Open(%q) error = %v", dbPath, err)
	}
	newLeaf := func(title string) model.Issue {
		issue, err := st.CreateIssue(ctx, storage.CreateIssueInput{
			Title: title, Topic: "work", Prefix: prefix, Placement: storage.RankBottom,
		})
		if err != nil {
			t.Fatalf("CreateIssue(%q) error = %v", title, err)
		}
		return issue
	}
	newLeaf("ready one") // ready
	newLeaf("ready two") // ready
	wip := newLeaf("in flight")
	if _, err := st.Apply(ctx, wip.ID, storage.Change{Action: model.Start{Assignee: "tester"}, Actor: "tester"}); err != nil {
		t.Fatalf("Apply(start) error = %v", err)
	}
	blocked := newLeaf("blocked")
	// blocked depends on the still-unfinished in-progress leaf, so it is blocked
	// without adding a second ready leaf that would perturb the asserted counts.
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{
		SrcID: blocked.ID, DstID: wip.ID, Type: "blocks", CreatedBy: "tester",
	}); err != nil {
		t.Fatalf("AddRelation(blocks) error = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}

	payload, err := json.Marshal(workspace.Config{WorkspaceID: workspaceID, IssuePrefix: prefix})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storageDir, "config.json"), payload, 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	return storageDir, prefix
}

// TestGatherCrossProjectRollupCountsWorkable is the happy-path guard for the core
// new data path: discovery through classification to counts. A real Discover-able
// store with a known backlog must roll up to Err==nil with the exact
// ready / in-flight / blocked counts and the config-derived prefix label — so a
// regression in classification, store integration, or count assignment is caught.
// [LAW:behavior-not-structure] Asserts the counts the feature promises, not how
// they are computed.
func TestGatherCrossProjectRollupCountsWorkable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LIT_CONFIG_GLOBAL_PATH", "")
	t.Setenv("LIT_CONFIG_PROJECT_PATH", "")

	// EvalSymlinks up front so the seed path, the config path, and the path
	// Discover derives from git are one spelling of the store, never two.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(root) error = %v", err)
	}
	repo := filepath.Join(root, "repoReal")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repoReal: %v", err)
	}
	gitInit(t, repo)
	_, wantPrefix := seedDiscoverableStore(t, repo, "real", "real-workspace-id")

	rows, err := gatherCrossProjectRollup(context.Background(), []string{root})
	if err != nil {
		t.Fatalf("gatherCrossProjectRollup() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("gatherCrossProjectRollup() returned %d rows, want 1:\n%+v", len(rows), rows)
	}
	row := rows[0]
	if row.Err != nil {
		t.Fatalf("row.Err = %v; want a clean read of the seeded store", row.Err)
	}
	if row.Ready != 2 || row.InFlight != 1 || row.Blocked != 1 {
		t.Fatalf("row counts = ready %d / in-flight %d / blocked %d; want 2 / 1 / 1", row.Ready, row.InFlight, row.Blocked)
	}
	if row.Label != wantPrefix {
		t.Fatalf("row.Label = %q; want the config issue prefix %q", row.Label, wantPrefix)
	}
}

// TestRunStoresCountsRendersRollup is the fold's acceptance: `stores --counts`
// routes the same discovery walk into the cross-project count rollup (the former
// `lit overview`), while bare `stores` still lists storage paths. Proves the flag
// wiring end-to-end, not just the rollup helper in isolation.
func TestRunStoresCountsRendersRollup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LIT_CONFIG_GLOBAL_PATH", "")
	t.Setenv("LIT_CONFIG_PROJECT_PATH", "")

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(root) error = %v", err)
	}
	repo := filepath.Join(root, "repoReal")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repoReal: %v", err)
	}
	gitInit(t, repo)
	_, wantPrefix := seedDiscoverableStore(t, repo, "real", "real-workspace-id")

	var counts bytes.Buffer
	if err := runStores(context.Background(), &counts, []string{"--counts", root}); err != nil {
		t.Fatalf("runStores --counts error = %v", err)
	}
	got := counts.String()
	for _, want := range []string{"PROJECT", "READY", "IN-FLIGHT", "BLOCKED", wantPrefix, "TOTAL"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stores --counts output %q missing %q", got, want)
		}
	}

	// Bare `stores` lists paths — no count table leaks in without the flag.
	var plain bytes.Buffer
	if err := runStores(context.Background(), &plain, []string{root}); err != nil {
		t.Fatalf("runStores (no --counts) error = %v", err)
	}
	if strings.Contains(plain.String(), "PROJECT") || strings.Contains(plain.String(), "TOTAL") {
		t.Fatalf("bare stores must list paths, not a count table:\n%s", plain.String())
	}
}

// lineContaining returns the single output line that contains sub, failing the
// test if none does — a small helper so the render assertions read as intent.
func lineContaining(t *testing.T, out, sub string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	t.Fatalf("no output line contains %q:\n%s", sub, out)
	return ""
}
