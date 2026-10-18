package cli

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// TestTheMirrorAloneKeepsTheHistoryBound drives alpha's writes past a small
// bound with the on-change mirror as the only push. The mirror's clone
// publishes the re-root, the live store follows it and shrinks, and the next
// write the mirror pushes sits one commit above the kept window.
func TestTheMirrorAloneKeepsTheHistoryBound(t *testing.T) {
	// Auto-sync stays off throughout: a write here spawns no detached mirror,
	// which would push from another process under the default bound.
	r, _ := newSharedCheckouts(t)
	isolateEvents(t)
	runCLIInDir(t, r.alpha, "sync", "push")
	seeded := boundRecordOf(t, lastEvent(t).Facts)["history_bound.height"].(float64)

	defaultBound := store.DefaultHistoryBound
	t.Cleanup(func() { store.DefaultHistoryBound = defaultBound })
	store.DefaultHistoryBound.ReRootAt = uint64(seeded) + 3
	store.DefaultHistoryBound.Keep = 2
	for _, title := range []string{"one", "two", "three"} {
		runCLIInDir(t, r.alpha, "new", "--title", title, "--topic", "demo", "--type", "task", "--parent", r.epic)
	}
	alphaWS, err := workspace.Resolve(r.alpha)
	if err != nil {
		t.Fatalf("resolve alpha: %v", err)
	}
	before := dirBytes(t, alphaWS.DatabasePath)

	runCLIInDir(t, r.alpha, "sync", backgroundMirrorSubcommand, "--parent-pid", "0")
	facts := lastEvent(t).Facts
	bound := boundRecordOf(t, facts)
	if bound["history_bound.fired"] != true || bound["history_bound.lease"] != "held" || bound["history_bound.error"] != nil || bound["reroot.kept"] != float64(2) {
		t.Fatalf("the mirror's bound = %v; want it fired, published under a held lease and keeping 2 commits", bound)
	}
	// The clone follows the re-root it published, and the live store follows
	// the same re-root when it receives the clone's push.
	follows, _ := facts["reroot.follows"].([]any)
	if len(follows) != 2 {
		t.Fatalf("reroot.follows = %v; want the clone's bound follow and the live store's receive", facts["reroot.follows"])
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
	if next := boundRecordOf(t, lastEvent(t).Facts); next["history_bound.fired"] != false || next["history_bound.height"] != float64(3) {
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
	seeded := boundRecordOf(t, lastEvent(t).Facts)["history_bound.height"].(float64)
	alphaWS, err := workspace.Resolve(r.alpha)
	if err != nil {
		t.Fatalf("resolve alpha: %v", err)
	}
	proved, err := store.ReadReceivedRefs(alphaWS.DatabasePath)
	if err != nil || proved == nil {
		t.Fatalf("the seeding push's received-refs record = %q (%v); want the advertisement it proved", proved, err)
	}

	defaultBound := store.DefaultHistoryBound
	t.Cleanup(func() { store.DefaultHistoryBound = defaultBound })
	store.DefaultHistoryBound.ReRootAt = uint64(seeded) + 1
	store.DefaultHistoryBound.Keep = 2
	runCLIInDir(t, r.alpha, "new", "--title", "one", "--topic", "demo", "--type", "task", "--parent", r.epic)
	runCLIInDir(t, r.alpha, "sync", "push")
	bound := boundRecordOf(t, lastEvent(t).Facts)
	if bound["history_bound.fired"] != true || bound["history_bound.lease"] != "held" {
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
	if trace.Metadata["head"] != bound["history_bound.head"] || reRoot == "" || !strings.Contains(trace.Metadata["after_push"], reRoot) || trace.Metadata["advertisement"] != "" {
		t.Fatalf("the push's trace metadata = %v; want the pushed head %v, after_push naming the re-root %q, and no advertisement", trace.Metadata, bound["history_bound.head"], reRoot)
	}
	if after, err := store.ReadReceivedRefs(alphaWS.DatabasePath); err != nil || string(after) != string(proved) {
		t.Fatalf("received-refs after the bound fired = %q (%v); want the seeding push's record %q left standing", after, err, proved)
	}
}

// boundRecordOf is the one history bound an event's facts carry.
func boundRecordOf(t *testing.T, facts map[string]any) map[string]any {
	t.Helper()
	records, _ := facts["history_bound.records"].([]any)
	if len(records) != 1 {
		t.Fatalf("history_bound.records = %v; want one bound", facts["history_bound.records"])
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
// of a cycle's facts to the step it gives the live store.
func TestTheLiveStoreLearnsWhereTheCycleLeftTheRemote(t *testing.T) {
	landed := pushedHead{remote: "origin", branch: "master", head: "abc"}
	followed, reRooted, both := landed, landed, landed
	followed.followedReRoot = true
	reRooted.reRootedAfter = true
	both.followedReRoot, both.reRootedAfter = true, true
	for _, tc := range []struct {
		name   string
		pushed pushedHead
		want   liveStoreStep
	}{
		{"nothing landed", pushedHead{}, liveStoreWaits},
		{"an ordinary landed push", landed, liveStoreRecords},
		{"the clone followed a re-root", followed, liveStoreReceives},
		{"the bound re-rooted after, the clone not following", reRooted, liveStoreWaits},
		{"the bound re-rooted after, the clone following", both, liveStoreReceives},
	} {
		if got := tc.pushed.liveStoreStep(); got != tc.want {
			t.Errorf("%s: liveStoreStep() = %d; want %d", tc.name, got, tc.want)
		}
	}
}
