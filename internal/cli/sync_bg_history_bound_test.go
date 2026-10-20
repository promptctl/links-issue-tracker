package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestTheMirrorAloneKeepsTheHistoryBound drives alpha's writes past a small
// bound with the on-change mirror as the only push. The mirror's clone
// publishes the re-root, the live store follows it and shrinks, and the next
// write the mirror pushes sits one commit above the kept window.
func TestTheMirrorAloneKeepsTheHistoryBound(t *testing.T) {
	// Auto-sync stays off throughout: a write here spawns no detached mirror,
	// which would push from another process, racing the pushes this test
	// sequences.
	r, _ := newSharedCheckouts(t)
	isolateEvents(t)
	runCLIInDir(t, r.alpha, "sync", "push")
	seeded := boundRecordOf(t, lastEvent(t))["history_bound.height"].(float64)

	setHistoryWindow(t, r.alpha, uint64(seeded)+3, 2)
	for _, title := range []string{"one", "two", "three"} {
		runCLIInDir(t, r.alpha, "new", "--title", title, "--topic", "demo", "--type", "task", "--parent", r.epic)
	}
	alphaWS, err := workspace.Resolve(r.alpha)
	if err != nil {
		t.Fatalf("resolve alpha: %v", err)
	}
	before := dirBytes(t, alphaWS.DatabasePath)

	runCLIInDir(t, r.alpha, "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
	ev := lastEvent(t)
	bound := boundRecordOf(t, ev)
	if bound["history_bound.fired"] != true || bound["history_bound.lease"] != "held" || bound["history_bound.error"] != nil || bound["reroot.kept"] != float64(2) {
		t.Fatalf("the mirror's bound = %v; want it fired, published under a held lease and keeping 2 commits", bound)
	}
	// The clone follows the re-root it published, and the live store follows
	// the same re-root when it receives the clone's push.
	follows := ev.Occurrences["reroot.follows"]
	if len(follows) != 2 {
		t.Fatalf("reroot.follows = %v; want the clone's bound follow and the live store's receive", follows)
	}
	for i, entry := range []string{"bound", "receive"} {
		follow, _ := follows[i].(map[string]any)
		if follow["reroot.follow"] != entry || follow["reroot.head"] != bound["reroot.head"] {
			t.Fatalf("follow %d = %v; want follow=%s onto the published head %v", i, follow, entry, bound["reroot.head"])
		}
	}
	if head, _ := syncHeadAndBranch(t, r.alpha); head != bound["reroot.head"] {
		t.Fatalf("alpha's live head is %s; want the published re-root %v", head, bound["reroot.head"])
	}
	if after := dirBytes(t, alphaWS.DatabasePath); after >= before {
		t.Fatalf("alpha's live store is %d bytes after the bound, %d before; want it smaller", after, before)
	}

	runCLIInDir(t, r.alpha, "new", "--title", "four", "--topic", "demo", "--type", "task", "--parent", r.epic)
	runCLIInDir(t, r.alpha, "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
	if next := boundRecordOf(t, lastEvent(t)); next["history_bound.fired"] != false || next["history_bound.height"] != float64(3) {
		t.Fatalf("the next mirror's bound = %v; want the remote head one commit above the 2 kept, short of the bound", next)
	}
}

// TestAPushWhoseBoundFiredProvesNothing pushes alpha past the bound in the
// foreground. The bound's re-root and fetch go through the store's git
// mirror after the push, so the mirror holding the remote's head no longer
// shows the push wrote it, and the received-refs record the earlier push
// proved stands.
func TestAPushWhoseBoundFiredProvesNothing(t *testing.T) {
	r, _ := newSharedCheckouts(t)
	isolateEvents(t)
	runCLIInDir(t, r.alpha, "sync", "push")
	seeded := boundRecordOf(t, lastEvent(t))["history_bound.height"].(float64)
	alphaWS, err := workspace.Resolve(r.alpha)
	if err != nil {
		t.Fatalf("resolve alpha: %v", err)
	}
	proved, err := store.ReadReceivedRefs(alphaWS.DatabasePath)
	if err != nil || proved == nil {
		t.Fatalf("the seeding push's received-refs record = %q (%v); want the advertisement it proved", proved, err)
	}

	setHistoryWindow(t, r.alpha, uint64(seeded)+1, 2)
	runCLIInDir(t, r.alpha, "new", "--title", "one", "--topic", "demo", "--type", "task", "--parent", r.epic)
	runCLIInDir(t, r.alpha, "sync", "push")
	bound := boundRecordOf(t, lastEvent(t))
	if bound["history_bound.fired"] != true || bound["history_bound.lease"] != "held" || bound["history_bound.publication"] != "published" {
		t.Fatalf("the push's bound = %v; want it fired and published", bound)
	}
	// The trace keeps the commit the push sent and says why it carries no
	// proof.
	var trace syncTraceRecord
	for _, record := range readSyncTraces(t, alphaWS) {
		if strings.HasPrefix(record.Command, "lit sync push") {
			trace = record
		}
	}
	reRoot, _ := bound["reroot.head"].(string)
	if trace.Metadata["head"] != bound["history_bound.head"] || reRoot == "" || !strings.Contains(trace.Metadata["after_push"], reRoot) || !strings.HasSuffix(trace.Metadata["after_push"], ": published") || trace.Metadata["advertisement"] != "" {
		t.Fatalf("the push's trace metadata = %v; want the pushed head %v, after_push naming the re-root %q, and no advertisement", trace.Metadata, bound["history_bound.head"], reRoot)
	}
	if after, err := store.ReadReceivedRefs(alphaWS.DatabasePath); err != nil || string(after) != string(proved) {
		t.Fatalf("received-refs after the bound fired = %q (%v); want the seeding push's record %q left standing", after, err, proved)
	}
}

// boundRecordOf is the one history bound an event's occurrences carry.
func boundRecordOf(t *testing.T, ev telemetry.Event) map[string]any {
	t.Helper()
	records := ev.Occurrences["history_bound.records"]
	if len(records) != 1 {
		t.Fatalf("history_bound.records = %v; want one bound", records)
	}
	record, _ := records[0].(map[string]any)
	return record
}

// dirBytes is the size of every file under root.
func dirBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("measure %s: %v", root, err)
	}
	return total
}

// TestTheLiveStoreLearnsWhereTheCycleLeftTheRemote walks every combination
// of a cycle's facts to the step it gives the live store. A push whose bound
// never got its re-root to the remote, or got it there and found the remote
// without it, leaves the mirror recording the pushed head, as an ordinary
// push does; one the remote may carry the re-root in place of waits.
func TestTheLiveStoreLearnsWhereTheCycleLeftTheRemote(t *testing.T) {
	landed := func(reRoot storage.ReRootPublication, followed bool) pushedHead {
		return pushedHead{remote: "origin", branch: "master", head: "abc", reRoot: reRoot, followedReRoot: followed}
	}
	for _, tc := range []struct {
		name   string
		pushed pushedHead
		want   liveStoreStep
	}{
		{"nothing landed", pushedHead{}, liveStoreWaits},
		{"a landed push whose re-root never left the checkout", landed(storage.ReRootNotPushed, false), liveStoreRecords},
		{"a landed push whose re-root the remote does not carry", landed(storage.ReRootNotCarried, false), liveStoreRecords},
		{"a landed push whose re-root is possibly published", landed(storage.ReRootPossiblyPublished, false), liveStoreWaits},
		{"a landed push whose re-root is published", landed(storage.ReRootPublished, false), liveStoreWaits},
		{"the clone followed a peer's re-root", landed(storage.ReRootNotPushed, true), liveStoreReceives},
		{"the clone followed a re-root the remote does not carry as its own", landed(storage.ReRootNotCarried, true), liveStoreReceives},
		{"the clone followed, its re-root possibly published", landed(storage.ReRootPossiblyPublished, true), liveStoreReceives},
		{"the clone followed its published re-root", landed(storage.ReRootPublished, true), liveStoreReceives},
	} {
		if got := tc.pushed.liveStoreStep(); got != tc.want {
			t.Errorf("%s: liveStoreStep() = %d; want %d", tc.name, got, tc.want)
		}
	}
}

// The bound fails nothing, so an illegal sync.history_bound fails only the
// bound: the push lands, its output says the bound was not kept and why, and
// the event's history_bound.window_error carries the refusal, with no bound
// record beside it.
func TestAnIllegalHistoryWindowFailsTheBoundAndNotThePush(t *testing.T) {
	r, _ := newSharedCheckouts(t)
	pushed := extractTicketID(t, runCLIInDir(t, r.alpha, "new", "--title", "pushed", "--topic", "demo", "--type", "task", "--parent", r.epic))
	setHistoryWindow(t, r.alpha, 5, 5)
	isolateEvents(t)
	const refusal = "config: sync.history_bound: a history window keeps at least one commit and fewer than it re-roots at; re_root_at 5, keep 5 is not one"
	if out := runCLIInDir(t, r.alpha, "sync", "push"); !strings.Contains(out, "history bound not kept: "+refusal) {
		t.Fatalf("lit sync push under re_root_at 5, keep 5:\n%s\nwant the push landed and the bound not kept, naming the refusal", out)
	}
	ev := readEvents(t)[0]
	if got, _ := ev.Facts["history_bound.window_error"].(string); got != refusal {
		t.Fatalf("event %q history_bound.window_error = %v; want %q", ev.Event, ev.Facts["history_bound.window_error"], refusal)
	}
	if records, ok := ev.Occurrences["history_bound.records"]; !ok || len(records) != 0 {
		t.Fatalf("event %q history_bound.records = %v; want an empty list: no bound ran", ev.Event, ev.Occurrences["history_bound.records"])
	}
	runCLIInDir(t, r.bravo, "sync", "pull")
	runCLIInDir(t, r.bravo, "show", pushed)
}

// setHistoryWindow sets sync.history_bound in repo's project config,
// creating the file if no fixture wrote one, so every later push from repo
// keeps its history inside that window. The table it writes is always the
// file's last, so a later call replaces it rather than adding a second.
func setHistoryWindow(t *testing.T, repo string, reRootAt, keep uint64) {
	t.Helper()
	const table = "\n[sync.history_bound]\n"
	path := filepath.Join(repo, ".lit", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read project config %s: %v", path, err)
	}
	before, _, _ := strings.Cut(string(existing), table)
	content := fmt.Sprintf("%s%sre_root_at = %d\nkeep = %d\n", before, table, reRootAt, keep)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write sync.history_bound to %s: %v", path, err)
	}
}
