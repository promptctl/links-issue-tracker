package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// testUnfiled is the unfiled epic of a store whose top-level ids carry the
// prefix "test": the first four hex digits of SHA-256("test-unfiled"), pinned
// rather than recomputed.
const testUnfiled = "test-unfiled-cfe6"

// TestUnfiledEpicMatchesMigration00007 holds the epic HomeStranded mints to the
// row migration 00007 writes from the same data. Two checkouts that strand
// tickets — one by migrating, one by a sync or a restore — must hold one epic:
// a different birth instant would make them two tickets under one id, and
// different text a held prose merge.
func TestUnfiledEpicMatchesMigration00007(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, provider := newContainmentFixture(t, ctx)
	f.issue(t, ctx, seedRow{id: "acme-core-aa11", issueType: "epic", rank: "D"})
	f.issue(t, ctx, seedRow{id: "acme-core-bb22", issueType: "task", rank: "M"})
	if _, err := provider.UpTo(ctx, 7); err != nil {
		t.Fatalf("UpTo(7) error = %v", err)
	}
	type row struct {
		title, description, topic, issueType, rank, lane string
		priority                                         int
		createdAt, updatedAt                             time.Time
	}
	var migrated row
	var created, updated string
	if err := f.st.db.QueryRowContext(ctx, `SELECT title, description, topic, issue_type, item_rank, lane, priority, created_at, updated_at FROM issues WHERE id = ?`, unfiledID).
		Scan(&migrated.title, &migrated.description, &migrated.topic, &migrated.issueType, &migrated.rank, &migrated.lane, &migrated.priority, &created, &updated); err != nil {
		t.Fatalf("read the migrated unfiled epic: %v", err)
	}
	migrated.createdAt, migrated.updatedAt = mustParseTime(t, created), mustParseTime(t, updated)

	born := mustParseTime(t, f.now)
	loose, err := model.HydrateStatus(model.Issue{ID: "acme-core-bb22", Title: "issue acme-core-bb22", IssueType: model.TypeTask, Topic: "core", Rank: "M", CreatedAt: born, UpdatedAt: born}, model.StatusView{Value: model.StateOpen})
	if err != nil {
		t.Fatalf("HydrateStatus: %v", err)
	}
	homed, rehomed := storage.HomeStranded(model.Export{Issues: []model.Issue{
		{ID: "acme-core-aa11", Title: "issue acme-core-aa11", IssueType: model.TypeEpic, Topic: "core", Rank: "D", CreatedAt: born, UpdatedAt: born},
		loose,
	}})
	if want := []storage.Rehomed{{IssueID: "acme-core-bb22", Epic: unfiledID}}; !reflect.DeepEqual(rehomed, want) {
		t.Fatalf("rehomed = %+v, want %+v", rehomed, want)
	}
	var minted model.Issue
	for _, issue := range homed.Issues {
		if issue.ID == unfiledID {
			minted = issue
		}
	}
	got := row{title: minted.Title, description: minted.Description, topic: minted.Topic, issueType: string(minted.IssueType), rank: minted.Rank, lane: minted.Lane, priority: int(minted.Priority), createdAt: minted.CreatedAt, updatedAt: minted.UpdatedAt}
	if got != migrated {
		t.Errorf("minted unfiled epic differs from the row 00007 writes:\n minted %+v\nmigrate %+v", got, migrated)
	}
	if lane := f.lane(t, ctx, "acme-core-bb22"); lane != "acme-core-bb22" {
		t.Fatalf("00007 lane = %q; the comparison below assumes it makes the leaf its own lane", lane)
	}
	var edgeAt, edgeBy string
	if err := f.st.db.QueryRowContext(ctx, `SELECT created_at, created_by FROM relations WHERE src_id = ? AND dst_id = ?`, "acme-core-bb22", unfiledID).Scan(&edgeAt, &edgeBy); err != nil {
		t.Fatalf("read the migrated edge: %v", err)
	}
	for _, r := range homed.Relations {
		if r.SrcID == "acme-core-bb22" && (!r.CreatedAt.Equal(mustParseTime(t, edgeAt)) || r.CreatedBy != edgeBy) {
			t.Errorf("minted edge stamped %s by %q; 00007 stamps %s by %q", r.CreatedAt, r.CreatedBy, edgeAt, edgeBy)
		}
	}
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

// TestSyncReconcileHomesAChildOfAnEpicPurgedOnTheOtherSide drives route 3 of
// the containment gap end to end: A purges an epic while it is empty and
// pushes; B, not yet having heard, files a task under it. The merge applies the
// purge to both sides, which cuts the task's edge, so the reconcile lands the
// task under the unfiled epic and says so — and the push carries that to A.
func TestSyncReconcileHomesAChildOfAnEpicPurgedOnTheOtherSide(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rootA := migratedDoltDir(t)
	rootB := unrelatedDoltDir(t)
	remoteURL := "file://" + filepath.Join(t.TempDir(), "remote")

	doomed := seedReconcileRemote(t, ctx, rootA, remoteURL)
	createEpicAndPush(t, ctx, rootA, "kept")
	adoptRemote(t, ctx, rootB, remoteURL)
	purgeAndPush(t, ctx, rootA, doomed)

	stB := openOrFatal(t, ctx, rootB)
	child, err := stB.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "test", Title: "filed late", Topic: "topic", IssueType: "task", ParentID: doomed})
	if err != nil {
		t.Fatalf("CreateIssue(child of %s): %v", doomed, err)
	}
	if err := stB.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}

	syncB := openSyncOrFatal(t, ctx, rootB)
	if err := syncB.SyncFetch(ctx, "origin", false); err != nil {
		t.Fatalf("SyncFetch(B): %v", err)
	}
	res, err := syncB.SyncReconcile(ctx, "origin", "master")
	if err != nil {
		t.Fatalf("SyncReconcile(B): %v", err)
	}
	if res.State != storage.SyncReconcileLinearized {
		t.Fatalf("reconcile state = %q, want %q", res.State, storage.SyncReconcileLinearized)
	}
	if want := []storage.Rehomed{{IssueID: child.ID, Epic: testUnfiled}}; !reflect.DeepEqual(res.Rehomed, want) {
		t.Errorf("Rehomed = %+v, want %+v", res.Rehomed, want)
	}
	assertHomedUnder(t, ctx, syncB, child.ID, testUnfiled)
	if _, err := syncB.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(B): %v", err)
	}
	if err := syncB.Close(); err != nil {
		t.Fatalf("Close(B sync): %v", err)
	}

	syncA := openSyncOrFatal(t, ctx, rootA)
	defer func() { _ = syncA.Close() }()
	if _, err := syncA.SyncReceive(ctx, "origin", "master"); err != nil {
		t.Fatalf("SyncReceive(A): %v", err)
	}
	assertHomedUnder(t, ctx, syncA, child.ID, testUnfiled)
}

// TestRestoreHomesWhatTheBackupLeavesOutsideAnEpic drives route 4: a backup
// whose parent edge names an epic purged since it was taken, and a backup from
// before every ticket lived inside an epic. The restore lands each such ticket
// under the unfiled epic and returns the moves.
func TestRestoreHomesWhatTheBackupLeavesOutsideAnEpic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("parent purged since the backup", func(t *testing.T) {
		t.Parallel()
		st := openIssueStore(t, ctx)
		gone := createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "Gone", Topic: "topic", IssueType: "epic"})
		kept := createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "Kept", Topic: "topic", IssueType: "epic"})
		child := createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "Child", Topic: "topic", IssueType: "task", ParentID: gone.ID})
		backup, err := st.Export(ctx)
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: child.ID, ParentID: kept.ID, CreatedBy: "tester"}); err != nil {
			t.Fatalf("SetParent: %v", err)
		}
		if _, err := st.Purge(ctx, storage.PurgeInput{IssueID: gone.ID, Actor: "tester"}); err != nil {
			t.Fatalf("Purge(%s): %v", gone.ID, err)
		}

		rehomed, err := st.ReplaceFromExport(ctx, backup)
		if err != nil {
			t.Fatalf("ReplaceFromExport: %v", err)
		}
		if want := []storage.Rehomed{{IssueID: child.ID, Epic: testUnfiled}}; !reflect.DeepEqual(rehomed, want) {
			t.Errorf("rehomed = %+v, want %+v", rehomed, want)
		}
		assertHomedUnder(t, ctx, st, child.ID, testUnfiled)
	})

	t.Run("backup from before containment", func(t *testing.T) {
		t.Parallel()
		st := openIssueStore(t, ctx)
		home := createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "Home", Topic: "topic", IssueType: "epic"})
		loose := createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: "Loose", Topic: "topic", IssueType: "task", ParentID: home.ID})
		backup, err := st.Export(ctx)
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		backup.Relations = nil

		rehomed, err := st.ReplaceFromExport(ctx, backup)
		if err != nil {
			t.Fatalf("ReplaceFromExport: %v", err)
		}
		if want := []storage.Rehomed{{IssueID: loose.ID, Epic: testUnfiled}}; !reflect.DeepEqual(rehomed, want) {
			t.Errorf("rehomed = %+v, want %+v", rehomed, want)
		}
		assertHomedUnder(t, ctx, st, loose.ID, testUnfiled)
	})
}

func createOrFatal(t *testing.T, ctx context.Context, st *Store, in storage.CreateIssueInput) model.Issue {
	t.Helper()
	issue, err := st.CreateIssue(ctx, in)
	if err != nil {
		t.Fatalf("CreateIssue(%s): %v", in.Title, err)
	}
	return issue
}

func createEpicAndPush(t *testing.T, ctx context.Context, root, title string) {
	t.Helper()
	st := openOrFatal(t, ctx, root)
	createOrFatal(t, ctx, st, storage.CreateIssueInput{Prefix: "test", Title: title, Topic: "topic", IssueType: "epic"})
	if err := st.Close(); err != nil {
		t.Fatalf("Close(%s): %v", root, err)
	}
	sync := openSyncOrFatal(t, ctx, root)
	defer func() { _ = sync.Close() }()
	if _, err := sync.SyncPush(ctx, "origin", "master", false, false); err != nil {
		t.Fatalf("SyncPush(%s): %v", title, err)
	}
}

// assertHomedUnder fails unless child's one parent is epic, that epic is the
// unfiled epic, and Doctor reports nothing: no ticket is left outside an epic.
func assertHomedUnder(t *testing.T, ctx context.Context, st *Store, child, epic string) {
	t.Helper()
	if got := readParent(t, ctx, st, child); got != epic {
		t.Errorf("%s parent = %q, want %q", child, got, epic)
	}
	unfiled := getIssueOrFatal(t, ctx, st, epic)
	if unfiled.Title != storage.UnfiledTitle || unfiled.IssueType != model.TypeEpic {
		t.Errorf("%s = %q (%s), want the unfiled epic", epic, unfiled.Title, unfiled.IssueType)
	}
	report, err := st.Doctor(ctx)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if len(report.Errors) != 0 || len(report.Warnings) != 0 {
		t.Errorf("Doctor errors %v, warnings %v; want none", report.Errors, report.Warnings)
	}
}
