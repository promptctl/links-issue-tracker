package store

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestMigrationRenamesBlocksEdgesKeepingTheirMeaning pins v8 on a store whose
// relations CHECK carries an engine-generated name, as a pre-goose store's
// does: every pre-v8
// 'blocks' row becomes 'blocked-by' with its src and dst untouched, so the
// ticket that waited still waits — read back through the store, the waiter
// lists its blocker under BlockedBy. The other relation types pass through, the
// sealed set refuses the old spelling afterwards, and down restores 'blocks'
// row for row.
func TestMigrationRenamesBlocksEdgesKeepingTheirMeaning(t *testing.T) {
	ctx := context.Background()
	f, provider := newContainmentFixture(t, ctx)
	if _, err := provider.UpTo(ctx, 7); err != nil {
		t.Fatalf("UpTo(7) error = %v", err)
	}
	// A store created before goose declared the CHECK inline and unnamed, so
	// its engine-generated name differs per store; v8 must find it anyway.
	for _, stmt := range []string{
		`ALTER TABLE relations DROP CONSTRAINT relations_type_check`,
		`ALTER TABLE relations ADD CHECK (type IN ('blocks','parent-child','related-to'))`,
	} {
		if _, err := f.st.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("give relations a pre-goose CHECK (%s): %v", stmt, err)
		}
	}
	if name := relationsCheckName(t, ctx, f); name == "relations_type_check" {
		t.Fatalf("seeded CHECK name = %q, want an engine-generated one", name)
	}
	for _, id := range []string{"e-waiter", "e-blocker", "e-peer"} {
		f.issue(t, ctx, seedRow{id: id, issueType: "epic", rank: "a" + id, lane: id})
	}
	for _, edge := range [][3]string{
		{"e-waiter", "e-blocker", "blocks"},
		{"e-blocker", "e-peer", "related-to"},
	} {
		if _, err := f.st.db.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, ?, ?, 'seed')`, edge[0], edge[1], edge[2], f.now); err != nil {
			t.Fatalf("seed %v: %v", edge, err)
		}
	}

	if _, err := provider.UpTo(ctx, 8); err != nil {
		t.Fatalf("UpTo(8) error = %v", err)
	}
	wantUp := []string{"e-blocker e-peer related-to", "e-waiter e-blocker blocked-by"}
	if got := relationRows(t, ctx, f); !slices.Equal(got, wantUp) {
		t.Fatalf("relations after v8 = %v, want %v", got, wantUp)
	}
	detail, err := f.st.GetIssueDetail(ctx, "e-waiter")
	if err != nil {
		t.Fatalf("GetIssueDetail(e-waiter): %v", err)
	}
	if len(detail.BlockedBy) != 1 || detail.BlockedBy[0].ID != "e-blocker" {
		t.Fatalf("e-waiter blocked_by after v8 = %+v, want [e-blocker]", detail.BlockedBy)
	}
	if name := relationsCheckName(t, ctx, f); name != "relations_type_check" {
		t.Fatalf("relations CHECK after v8 = %q, want relations_type_check", name)
	}
	_, err = f.st.db.ExecContext(ctx, `INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES ('e-peer', 'e-waiter', 'blocks', ?, 'seed')`, f.now)
	if err == nil || !strings.Contains(err.Error(), "relations_type_check") {
		t.Fatalf("insert of a 'blocks' row after v8 error = %v, want relations_type_check to refuse it", err)
	}

	if _, err := provider.DownTo(ctx, 7); err != nil {
		t.Fatalf("DownTo(7) error = %v", err)
	}
	wantDown := []string{"e-blocker e-peer related-to", "e-waiter e-blocker blocks"}
	if got := relationRows(t, ctx, f); !slices.Equal(got, wantDown) {
		t.Fatalf("relations after down to v7 = %v, want %v", got, wantDown)
	}
}

// relationsCheckName is the name of the one CHECK constraint on relations.
func relationsCheckName(t *testing.T, ctx context.Context, f containmentFixture) string {
	t.Helper()
	var name string
	if err := f.st.db.QueryRowContext(ctx, `SELECT constraint_name FROM information_schema.table_constraints
		WHERE table_schema = DATABASE() AND table_name = 'relations' AND constraint_type = 'CHECK'`).Scan(&name); err != nil {
		t.Fatalf("read relations CHECK name: %v", err)
	}
	return name
}

func relationRows(t *testing.T, ctx context.Context, f containmentFixture) []string {
	t.Helper()
	rows, err := f.st.db.QueryContext(ctx, `SELECT src_id, dst_id, type FROM relations ORDER BY src_id, dst_id`)
	if err != nil {
		t.Fatalf("read relations: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var src, dst, typ string
		if err := rows.Scan(&src, &dst, &typ); err != nil {
			t.Fatalf("scan relation: %v", err)
		}
		out = append(out, src+" "+dst+" "+typ)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("relations rows: %v", err)
	}
	return out
}
