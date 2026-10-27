package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// containmentFixture is a store stepped back to schema v6 with a helper for
// seeding the pre-v7 shapes the migration has to home. Rows are written
// straight into the tables: the write boundary refuses every shape under test.
type containmentFixture struct {
	st  *Store
	now string
}

func newContainmentFixture(t *testing.T, ctx context.Context) (containmentFixture, *goose.Provider) {
	t.Helper()
	st, err := Open(ctx, testEnv(), testStderr(), migratedDoltDir(t), "test-workspace-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	provider, err := newGooseProvider(st.db)
	if err != nil {
		t.Fatalf("newGooseProvider() error = %v", err)
	}
	if _, err := provider.DownTo(ctx, 6); err != nil {
		t.Fatalf("DownTo(6) error = %v", err)
	}
	return containmentFixture{st: st, now: time.Now().UTC().Format(time.RFC3339Nano)}, provider
}

type seedRow struct {
	id, issueType, rank, lane string
	deleted                   bool
	// topic and title default to "core" and "issue <id>"; the synced-epic
	// case seeds the unfiled epic as the migration writes it.
	topic, title string
}

// topicAndTitle is the row's topic and title with their defaults filled in.
func (row seedRow) topicAndTitle() (string, string) {
	topic, title := row.topic, row.title
	if topic == "" {
		topic = "core"
	}
	if title == "" {
		title = "issue " + row.id
	}
	return topic, title
}

func (f containmentFixture) issue(t *testing.T, ctx context.Context, row seedRow) {
	t.Helper()
	var status, deletedAt any
	if row.issueType != "epic" {
		status = "open"
	}
	if row.deleted {
		deletedAt = f.now
	}
	topic, title := row.topicAndTitle()
	if _, err := f.st.db.ExecContext(ctx, `INSERT INTO issues(id, title, description, status, priority, issue_type, topic, assignee, item_rank, lane, created_at, updated_at, deleted_at)
		VALUES (?, ?, '', ?, 0, ?, ?, '', ?, ?, ?, ?, ?)`,
		row.id, title, status, row.issueType, topic, row.rank, row.lane, f.now, f.now, deletedAt); err != nil {
		t.Fatalf("seed issue %s: %v", row.id, err)
	}
}

func (f containmentFixture) parent(t *testing.T, ctx context.Context, child, parent string) {
	t.Helper()
	if _, err := f.st.db.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'parent-child', ?, 'seed')`, child, parent, f.now); err != nil {
		t.Fatalf("seed parent %s -> %s: %v", child, parent, err)
	}
}

func (f containmentFixture) lane(t *testing.T, ctx context.Context, id string) string {
	t.Helper()
	var lane string
	if err := f.st.db.QueryRowContext(ctx, `SELECT lane FROM issues WHERE id = ?`, id).Scan(&lane); err != nil {
		t.Fatalf("read lane of %s: %v", id, err)
	}
	return lane
}

// unfiledID is the id the migration mints for a store whose top-level ids
// mostly carry the prefix "acme": "-unfiled-" and the first four hex digits of
// SHA-256("acme-unfiled"). The literal is pinned rather than recomputed so the
// test asserts the contract two checkouts must agree on, not the formula.
const unfiledID = "acme-unfiled-0bf7"

// shapeSeeder writes one ticket or one parent edge of the containment fixture.
// The migration test writes them as rows at schema v6; the HomeStranded test
// builds an export of them. Both then answer to containmentHomes and
// containmentLanes, so the migration and the homing a sync or a restore applies
// are held to one rule over one set of shapes. [LAW:one-source-of-truth]
type shapeSeeder interface {
	issue(row seedRow)
	parent(child, parent string)
}

// fixtureSeeder writes the shapes into a store at schema v6.
type fixtureSeeder struct {
	t   *testing.T
	ctx context.Context
	f   containmentFixture
}

func (s fixtureSeeder) issue(row seedRow)           { s.f.issue(s.t, s.ctx, row) }
func (s fixtureSeeder) parent(child, parent string) { s.f.parent(s.t, s.ctx, child, parent) }

// seedContainmentShapes writes every pre-v7 shape the rule has to home.
func seedContainmentShapes(s shapeSeeder) {
	// Controls: an epic with a leaf child, an epic inside an epic, and a leaf
	// inside the nested epic. None of these moves.
	s.issue(seedRow{id: "acme-core-aa11", issueType: "epic", rank: "D"})
	s.issue(seedRow{id: "acme-core-aa11.1", issueType: "task", rank: "V", lane: "left"})
	s.parent("acme-core-aa11.1", "acme-core-aa11")
	s.issue(seedRow{id: "acme-core-aa11.2", issueType: "epic", rank: "W"})
	s.parent("acme-core-aa11.2", "acme-core-aa11")
	s.issue(seedRow{id: "acme-core-aa11.2.1", issueType: "bug", rank: "V"})
	s.parent("acme-core-aa11.2.1", "acme-core-aa11.2")

	// Top-level leaves, live and deleted. The live ones' keys decide the
	// unfiled epic's rank: "B" is the best of them, so the epic takes "B0", the
	// next key after it, and "B" stays with its leaf. The deleted one's "A" would be better and does not count, since work in the
	// trash frames nothing. One top-level id carries another prefix so the
	// majority rule has something to decide.
	s.issue(seedRow{id: "acme-core-bb22", issueType: "task", rank: "M"})
	s.issue(seedRow{id: "acme-core-cc33", issueType: "bug", rank: "B"})
	s.issue(seedRow{id: "zed-core-dd44", issueType: "chore", rank: "A", deleted: true})

	// A leaf under a leaf under an epic: the child moves up to the epic.
	s.issue(seedRow{id: "acme-core-ee55", issueType: "epic", rank: "E"})
	s.issue(seedRow{id: "acme-core-ee55.1", issueType: "task", rank: "V"})
	s.parent("acme-core-ee55.1", "acme-core-ee55")
	s.issue(seedRow{id: "acme-core-ee55.1.1", issueType: "task", rank: "V"})
	s.parent("acme-core-ee55.1.1", "acme-core-ee55.1")

	// Two leaves deep: both move up to the epic, the deeper one climbing past
	// the shallower.
	s.issue(seedRow{id: "acme-core-ff66", issueType: "epic", rank: "F"})
	s.issue(seedRow{id: "t-deep-a", issueType: "task", rank: "V"})
	s.parent("t-deep-a", "acme-core-ff66")
	s.issue(seedRow{id: "t-deep-b", issueType: "task", rank: "V"})
	s.parent("t-deep-b", "t-deep-a")
	s.issue(seedRow{id: "t-deep-c", issueType: "chore", rank: "V"})
	s.parent("t-deep-c", "t-deep-b")

	// A leaf under a top-level leaf: no epic above either, so both go unfiled.
	s.issue(seedRow{id: "t-top-leaf", issueType: "task", rank: "N"})
	s.issue(seedRow{id: "t-under", issueType: "task", rank: "V"})
	s.parent("t-under", "t-top-leaf")

	// An epic under a top-level leaf: no epic is above it, and an epic may be
	// top-level, so it loses the leaf edge and stays at the top level with its
	// own child, while the leaf goes unfiled.
	s.issue(seedRow{id: "t-top-leaf2", issueType: "feature", rank: "P"})
	s.issue(seedRow{id: "t-nested-epic", issueType: "epic", rank: "V"})
	s.parent("t-nested-epic", "t-top-leaf2")
	s.issue(seedRow{id: "t-nested-epic.1", issueType: "task", rank: "V"})
	s.parent("t-nested-epic.1", "t-nested-epic")

	// A loop of leaves: the climb meets the depth cut, both go unfiled, and
	// the loop is gone with them.
	s.issue(seedRow{id: "t-cyc-a", issueType: "task", rank: "V"})
	s.issue(seedRow{id: "t-cyc-b", issueType: "task", rank: "V"})
	s.parent("t-cyc-a", "t-cyc-b")
	s.parent("t-cyc-b", "t-cyc-a")

	// Restored data: a loop of leaves with a fork in it. Each lap through the
	// fork doubles the ways up, so a climb that kept every way would hold some
	// 2^32 rows by the depth cut; kept as a set, it ends, and the child and the
	// loop all go unfiled.
	s.issue(seedRow{id: "t-fl-1", issueType: "task", rank: "V"})
	s.issue(seedRow{id: "t-fl-2", issueType: "task", rank: "V"})
	s.issue(seedRow{id: "t-fl-3", issueType: "task", rank: "V"})
	s.issue(seedRow{id: "t-fl-child", issueType: "bug", rank: "V"})
	s.parent("t-fl-1", "t-fl-2")
	s.parent("t-fl-1", "t-fl-3")
	s.parent("t-fl-2", "t-fl-1")
	s.parent("t-fl-3", "t-fl-1")
	s.parent("t-fl-child", "t-fl-1")

	// Restored data: a child with two leaf parents in two epics. The climb
	// forks, both forks end on an epic at the same depth, and the lower id
	// wins; the other edge goes, so one parent remains.
	s.issue(seedRow{id: "t-e1", issueType: "epic", rank: "G"})
	s.issue(seedRow{id: "t-l1", issueType: "task", rank: "V"})
	s.parent("t-l1", "t-e1")
	s.issue(seedRow{id: "t-e2", issueType: "epic", rank: "H"})
	s.issue(seedRow{id: "t-l2", issueType: "task", rank: "V"})
	s.parent("t-l2", "t-e2")
	s.issue(seedRow{id: "t-fork", issueType: "task", rank: "V"})
	s.parent("t-fork", "t-l1")
	s.parent("t-fork", "t-l2")

	// Restored data: a child with two leaf parents, one under an epic and
	// one under a leaf that goes nowhere. The dead end is as deep as the epic
	// and sorts before it; the epic is the destination all the same, since
	// it is the nearest epic any fork reached.
	s.issue(seedRow{id: "t-e5", issueType: "epic", rank: "L"})
	s.issue(seedRow{id: "t-l5", issueType: "task", rank: "V"})
	s.parent("t-l5", "t-e5")
	s.issue(seedRow{id: "t-d2", issueType: "task", rank: "V"})
	s.issue(seedRow{id: "t-d1", issueType: "task", rank: "V"})
	s.parent("t-d1", "t-d2")
	s.issue(seedRow{id: "t-fork2", issueType: "task", rank: "V"})
	s.parent("t-fork2", "t-l5")
	s.parent("t-fork2", "t-d1")

	// Restored data: a child holding an epic parent and a leaf parent in
	// another epic. It is already home, so it does not move: the leaf edge
	// goes, the epic edge stays, and its lane is left alone.
	s.issue(seedRow{id: "t-e3", issueType: "epic", rank: "I"})
	s.issue(seedRow{id: "t-e4", issueType: "epic", rank: "J"})
	s.issue(seedRow{id: "t-l4", issueType: "task", rank: "V"})
	s.parent("t-l4", "t-e4")
	s.issue(seedRow{id: "t-held", issueType: "task", rank: "V", lane: "right"})
	s.parent("t-held", "t-e3")
	s.parent("t-held", "t-l4")

	// Restored data: a loop through an epic. The leaf keeps its epic; the
	// epic's climb comes back to itself, so it is given no edge and comes out
	// at the top level, where an epic may live.
	s.issue(seedRow{id: "t-loop-epic", issueType: "epic", rank: "K"})
	s.issue(seedRow{id: "t-loop-leaf", issueType: "task", rank: "V"})
	s.parent("t-loop-leaf", "t-loop-epic")
	s.parent("t-loop-epic", "t-loop-leaf")
}

// containmentHomes is each seeded ticket's parent once the shapes are homed,
// empty for the top level.
var containmentHomes = map[string]string{
	"acme-core-aa11":     "",
	"acme-core-aa11.1":   "acme-core-aa11",
	"acme-core-aa11.2":   "acme-core-aa11",
	"acme-core-aa11.2.1": "acme-core-aa11.2",
	"acme-core-bb22":     unfiledID,
	"acme-core-cc33":     unfiledID,
	"zed-core-dd44":      unfiledID,
	"acme-core-ee55.1":   "acme-core-ee55",
	"acme-core-ee55.1.1": "acme-core-ee55",
	"t-deep-a":           "acme-core-ff66",
	"t-deep-b":           "acme-core-ff66",
	"t-deep-c":           "acme-core-ff66",
	"t-top-leaf":         unfiledID,
	"t-under":            unfiledID,
	"t-top-leaf2":        unfiledID,
	"t-nested-epic":      "",
	"t-nested-epic.1":    "t-nested-epic",
	"t-cyc-a":            unfiledID,
	"t-cyc-b":            unfiledID,
	"t-fl-1":             unfiledID,
	"t-fl-2":             unfiledID,
	"t-fl-3":             unfiledID,
	"t-fl-child":         unfiledID,
	"t-l1":               "t-e1",
	"t-l2":               "t-e2",
	"t-fork":             "t-e1",
	"t-l4":               "t-e4",
	"t-held":             "t-e3",
	"t-l5":               "t-e5",
	"t-d2":               unfiledID,
	"t-d1":               unfiledID,
	"t-fork2":            "t-e5",
	"t-loop-leaf":        "t-loop-epic",
	"t-loop-epic":        "",
	unfiledID:            "",
}

// containmentLanes is the lane of each seeded ticket whose lane the homing
// sets or must leave alone.
var containmentLanes = map[string]string{
	"acme-core-aa11.1":   "left",
	"acme-core-aa11.2.1": "",
	"acme-core-bb22":     "acme-core-bb22",
	"acme-core-ee55.1":   "",
	"acme-core-ee55.1.1": "acme-core-ee55.1.1",
	"t-deep-c":           "t-deep-c",
	"t-under":            "t-under",
	"t-nested-epic":      "t-nested-epic",
	"t-nested-epic.1":    "",
	"t-fork":             "t-fork",
	"t-held":             "right",
}

// TestContainmentMigrationHomesEveryLeaf exercises 00007's Up against every
// shape it names and the Down against the result. After Up, every leaf has a
// parent and every parent is an epic: a child under a leaf sits under the
// nearest epic above it, and a leaf with no epic above it sits under the
// store's unfiled epic, each as its own lane. After Down, the unfiled epic is
// gone and its children are top-level again, while a child Up moved under an
// epic ancestor stays there, the loss the Down contract states.
func TestContainmentMigrationHomesEveryLeaf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, provider := newContainmentFixture(t, ctx)

	seedContainmentShapes(fixtureSeeder{t: t, ctx: ctx, f: f})

	if _, err := provider.UpTo(ctx, 7); err != nil {
		t.Fatalf("UpTo(7) error = %v", err)
	}

	for id, want := range containmentHomes {
		if got := readParent(t, ctx, f.st, id); got != want {
			t.Errorf("after Up: %s parent = %q, want %q", id, got, want)
		}
	}
	// Every leaf has a parent and every parent is an epic, stated over the
	// whole table rather than row by row, so a shape the seed forgot would
	// still be caught.
	assertContainmentHolds(t, ctx, f.st)

	for id, want := range containmentLanes {
		if got := f.lane(t, ctx, id); got != want {
			t.Errorf("after Up: %s lane = %q, want %q", id, got, want)
		}
	}

	var (
		status, closedAt sql.NullString
		issueType, topic string
		rank, lane       string
		createdAt        string
		priority         int
	)
	if err := f.st.db.QueryRowContext(ctx, `SELECT status, closed_at, issue_type, topic, item_rank, lane, created_at, priority FROM issues WHERE id = ?`, unfiledID).
		Scan(&status, &closedAt, &issueType, &topic, &rank, &lane, &createdAt, &priority); err != nil {
		t.Fatalf("read the unfiled epic: %v", err)
	}
	if status.Valid || closedAt.Valid || issueType != "epic" || topic != "unfiled" || lane != "" || priority != 0 {
		t.Errorf("unfiled epic row = status %v closed_at %v type %q topic %q lane %q priority %d; want an open epic of topic unfiled in the default lane at normal priority", status, closedAt, issueType, topic, lane, priority)
	}
	if rank != "B0" {
		t.Errorf("unfiled epic rank = %q, want %q, the next key after the best key among the live top-level leaves it absorbed", rank, "B0")
	}
	var holders int
	if err := f.st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE item_rank = ?`, rank).Scan(&holders); err != nil {
		t.Fatalf("count holders of the unfiled epic's rank: %v", err)
	}
	if holders != 1 {
		t.Errorf("%d rows hold the unfiled epic's rank %q; want the epic alone, since a rank orders one row", holders, rank)
	}
	if createdAt != "2026-10-12T00:00:00Z" {
		t.Errorf("unfiled epic created_at = %q, want the fixed authoring date", createdAt)
	}

	if _, err := provider.DownTo(ctx, 6); err != nil {
		t.Fatalf("DownTo(6) error = %v", err)
	}
	wantRestored := map[string]string{
		"acme-core-bb22":     "",
		"acme-core-cc33":     "",
		"zed-core-dd44":      "",
		"t-top-leaf":         "",
		"t-under":            "",
		"t-top-leaf2":        "",
		"t-nested-epic":      "",
		"t-cyc-a":            "",
		"t-cyc-b":            "",
		"acme-core-ee55.1.1": "acme-core-ee55",
		"t-deep-c":           "acme-core-ff66",
		"t-fork":             "t-e1",
		"t-held":             "t-e3",
		"t-loop-leaf":        "t-loop-epic",
		"t-loop-epic":        "",
		"acme-core-aa11.1":   "acme-core-aa11",
	}
	for id, want := range wantRestored {
		if got := readParent(t, ctx, f.st, id); got != want {
			t.Errorf("after Down: %s parent = %q, want %q", id, got, want)
		}
	}
	var unfiledRows int
	if err := f.st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE id = ?`, unfiledID).Scan(&unfiledRows); err != nil {
		t.Fatalf("probe the unfiled epic: %v", err)
	}
	if unfiledRows != 0 {
		t.Errorf("after Down: the unfiled epic is still present")
	}
	for _, id := range []string{"acme-core-bb22", "t-under", "t-fork"} {
		if got := f.lane(t, ctx, id); got != "" {
			t.Errorf("after Down: %s lane = %q, want the default lane", id, got)
		}
	}
	if got := f.lane(t, ctx, "acme-core-aa11.1"); got != "left" {
		t.Errorf("after Down: a lane that was never the id was reset to %q", got)
	}
}

// TestContainmentMigrationIsIdempotentAcrossCheckouts pins the sync case: a
// checkout that received the unfiled epic from a peer that migrated first
// mints nothing and homes its own leaves under the epic it already holds. A
// store with nothing to home mints no epic at all.
func TestContainmentMigrationIsIdempotentAcrossCheckouts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("epic already received through sync", func(t *testing.T) {
		t.Parallel()
		f, provider := newContainmentFixture(t, ctx)
		f.issue(t, ctx, seedRow{id: "acme-core-aa11", issueType: "epic", rank: "D"})
		f.issue(t, ctx, seedRow{id: unfiledID, issueType: "epic", rank: "Q", topic: "unfiled", title: "Unfiled tickets"})
		f.issue(t, ctx, seedRow{id: "acme-core-bb22", issueType: "task", rank: "M"})
		if _, err := provider.UpTo(ctx, 7); err != nil {
			t.Fatalf("UpTo(7) error = %v", err)
		}
		if got := readParent(t, ctx, f.st, "acme-core-bb22"); got != unfiledID {
			t.Errorf("parent = %q, want the epic the peer minted, %q", got, unfiledID)
		}
		var rank string
		if err := f.st.db.QueryRowContext(ctx, `SELECT item_rank FROM issues WHERE id = ?`, unfiledID).Scan(&rank); err != nil {
			t.Fatalf("read the unfiled epic: %v", err)
		}
		if rank != "Q" {
			t.Errorf("unfiled epic rank = %q, want %q: the row the peer wrote is kept, not rewritten", rank, "Q")
		}
		var epics int
		if err := f.st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE topic = 'unfiled'`).Scan(&epics); err != nil {
			t.Fatalf("count unfiled epics: %v", err)
		}
		if epics != 1 {
			t.Errorf("unfiled epics = %d, want exactly one", epics)
		}
		assertContainmentHolds(t, ctx, f.st)
	})

	t.Run("nothing to home mints nothing", func(t *testing.T) {
		t.Parallel()
		f, provider := newContainmentFixture(t, ctx)
		f.issue(t, ctx, seedRow{id: "acme-core-aa11", issueType: "epic", rank: "D"})
		f.issue(t, ctx, seedRow{id: "acme-core-aa11.1", issueType: "task", rank: "V"})
		f.parent(t, ctx, "acme-core-aa11.1", "acme-core-aa11")
		if _, err := provider.UpTo(ctx, 7); err != nil {
			t.Fatalf("UpTo(7) error = %v", err)
		}
		var epics int
		if err := f.st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE topic = 'unfiled'`).Scan(&epics); err != nil {
			t.Fatalf("count unfiled epics: %v", err)
		}
		if epics != 0 {
			t.Errorf("unfiled epics = %d, want none: every leaf already had an epic", epics)
		}
		if got := f.lane(t, ctx, "acme-core-aa11.1"); got != "" {
			t.Errorf("an untouched child's lane = %q, want it left alone", got)
		}
	})

	t.Run("no readable prefix falls back to lit", func(t *testing.T) {
		t.Parallel()
		f, provider := newContainmentFixture(t, ctx)
		f.issue(t, ctx, seedRow{id: "t-loose", issueType: "task", rank: "M"})
		if _, err := provider.UpTo(ctx, 7); err != nil {
			t.Fatalf("UpTo(7) error = %v", err)
		}
		if got := readParent(t, ctx, f.st, "t-loose"); got != "lit-unfiled-e2be" {
			t.Errorf("parent = %q, want the epic minted under the fallback prefix, %q", got, "lit-unfiled-e2be")
		}
	})
}

// assertContainmentHolds states the rule over the whole table: no leaf
// without a parent edge, and no parent edge onto a leaf.
func assertContainmentHolds(t *testing.T, ctx context.Context, st *Store) {
	t.Helper()
	var loose, leafParents int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues i WHERE i.issue_type <> 'epic' AND NOT EXISTS (SELECT 1 FROM relations r WHERE r.src_id = i.id AND r.type = 'parent-child')`).Scan(&loose); err != nil {
		t.Fatalf("count loose leaves: %v", err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM relations r JOIN issues p ON p.id = r.dst_id WHERE r.type = 'parent-child' AND p.issue_type <> 'epic'`).Scan(&leafParents); err != nil {
		t.Fatalf("count leaf parents: %v", err)
	}
	if loose != 0 || leafParents != 0 {
		t.Errorf("containment: %d leaves without a parent and %d edges onto a leaf parent; want none of either", loose, leafParents)
	}
}
