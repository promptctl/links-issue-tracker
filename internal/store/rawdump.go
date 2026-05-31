package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// RawDump is a schema-assumption-free snapshot of every table in a workspace's
// Dolt database, read below the migration gate. It is the foundation of the
// data lifeboat (links-recovery-j0vl): the one reader that can release a
// workspace's application data from ANY schema version, including one that
// store.Open() refuses.
//
// [LAW:types-are-the-program] The shape is exactly a set of SQL result sets —
// each table is its ordered column list plus its rows as positional cells.
// There is no per-table struct and no "known columns" list, so "I assumed a
// column that does not exist" is unrepresentable. A future migration that adds
// or removes columns changes the data the dump carries, never the code that
// produces it.
type RawDump struct {
	WorkspaceID string     `json:"workspace_id"`
	Tables      []RawTable `json:"tables"`
}

// RawTable is one table's complete contents. Columns preserves catalog order so
// a positional row cell is unambiguously attributable to a column; Rows is
// always non-nil (an empty table is [], never null) so the artifact is total.
type RawTable struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// DumpRaw opens the Dolt database read-only and dumps every table and row
// without running migrations.
//
// [LAW:locality-or-seam] This is the below-the-gate seam. Every other entry
// point routes through store.Open -> migrate(), and migrate() is exactly what
// refuses a too-new or genuinely-incompatible workspace. DumpRaw reuses the
// connection primitive (openStoreConnection) but never invokes migrate, so it
// cannot land in the deadend the gate creates.
//
// [LAW:single-enforcer] The workspace shared lock is acquired here for the same
// reason store.OpenForRead acquires it: it excludes `lit snapshots restore`,
// which rotates the Dolt directory under an exclusive hold. Reading below the
// gate does not exempt the read from that coordination.
//
// Read-only on the broken DB is the safety property: the worst case is a read
// error, with the user's data still intact on disk.
func DumpRaw(ctx context.Context, doltRootDir string, workspaceID string) (_ RawDump, err error) {
	if err := validateOpenArgs(doltRootDir, workspaceID); err != nil {
		return RawDump{}, err
	}
	release, err := acquireWorkspaceShared(ctx, doltRootDir)
	if err != nil {
		return RawDump{}, err
	}
	// [LAW:no-silent-fallbacks] A release failure is rare but real and leaves
	// the workspace stuck busy for subsequent commands; surface it via the
	// named return joined with any read error rather than discarding it.
	defer func() {
		if relErr := release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()
	if _, statErr := os.Stat(doltRootDir); statErr != nil {
		// [LAW:no-silent-fallbacks] Only ENOENT means "uninitialized"; every
		// other stat error is its own failure mode the operator needs to see.
		if errors.Is(statErr, os.ErrNotExist) {
			return RawDump{}, fmt.Errorf("repository not initialized with lit — run 'lit init' first")
		}
		return RawDump{}, fmt.Errorf("stat database dir: %w", statErr)
	}
	s, err := openStoreConnection(doltRootDir, workspaceID)
	if err != nil {
		return RawDump{}, err
	}
	defer func() {
		if closeErr := s.db.Close(); closeErr != nil && !errors.Is(closeErr, context.Canceled) {
			err = errors.Join(err, closeErr)
		}
	}()
	names, err := listTables(ctx, s.db)
	if err != nil {
		return RawDump{}, err
	}
	tables := make([]RawTable, 0, len(names))
	for _, name := range names {
		table, err := dumpTable(ctx, s.db, name)
		if err != nil {
			return RawDump{}, err
		}
		tables = append(tables, table)
	}
	return RawDump{WorkspaceID: workspaceID, Tables: tables}, nil
}

// listTables returns every base table in the database in deterministic
// (catalog name) order. It reads the same information_schema catalog the
// baseline-shape verifier uses, so the dump enumerates exactly what the engine
// reports without a hand-maintained table list to drift.
func listTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY table_name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan table name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tables: %w", err)
	}
	return names, nil
}

// dumpTable reads every column and every row of one table generically — the
// column set is discovered from the result, never assumed.
//
// [LAW:types-are-the-program] Scanning into []any and normalizing the driver's
// []byte text/blob values to string is the only place a value's Go type is
// touched; the lit domain is text and numbers, so this loses nothing and keeps
// the artifact portable (raw bytes have no faithful JSON form). NULL scans as
// nil and serializes as JSON null, distinct from an empty string.
func dumpTable(ctx context.Context, db *sql.DB, name string) (RawTable, error) {
	rows, err := db.QueryContext(ctx, "SELECT * FROM `"+name+"`")
	if err != nil {
		return RawTable{}, fmt.Errorf("select %q: %w", name, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return RawTable{}, fmt.Errorf("columns %q: %w", name, err)
	}
	table := RawTable{Name: name, Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return RawTable{}, fmt.Errorf("scan row of %q: %w", name, err)
		}
		for i, cell := range cells {
			if b, ok := cell.([]byte); ok {
				cells[i] = string(b)
			}
		}
		table.Rows = append(table.Rows, cells)
	}
	if err := rows.Err(); err != nil {
		return RawTable{}, fmt.Errorf("iterate rows of %q: %w", name, err)
	}
	return table, nil
}
