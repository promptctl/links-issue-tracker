package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// schemaSnapshotPath is the canonical, checked-in dump of the converged
// schema. Lives next to the source files (not under testdata/) because it
// is a *normative* artifact: a migration body landing without an updated
// snapshot must fail CI loudly. Hiding it under testdata/ obscures that
// invariant.
const schemaSnapshotPath = "schema_snapshot.sql"

// regenSchemaSnapshotEnv is the env var that switches the snapshot test
// from "compare and fail on drift" to "rewrite the snapshot file." When a
// migration body intentionally changes the resulting schema, regenerate
// the snapshot in the same commit:
//
//	LIT_REGEN_SCHEMA_SNAPSHOT=1 go test ./internal/store -run TestSchemaSnapshotMatches
const regenSchemaSnapshotEnv = "LIT_REGEN_SCHEMA_SNAPSHOT"

// TestSchemaSnapshotMatches is the drift gate. It applies every registered
// migration on a fresh workspace, dumps the schema, and compares against
// the checked-in snapshot. Any divergence — a migration body changing a
// column type, a new index, a renamed constraint — fails the test until
// the snapshot is regenerated.
//
// Acceptance for links-schema-3ix.3: this test plus the snapshot file
// itself (schema_snapshot.sql) form the canary. CI runs this test; if a
// migration changes the converged schema, the test fails with a diff.
func TestSchemaSnapshotMatches(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "dolt"), "snapshot-test-id")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer st.Close()

	actual, err := dumpSchemaSnapshot(ctx, st.db)
	if err != nil {
		t.Fatalf("dumpSchemaSnapshot() error = %v", err)
	}

	if os.Getenv(regenSchemaSnapshotEnv) == "1" {
		if err := os.WriteFile(schemaSnapshotPath, []byte(actual), 0o644); err != nil {
			t.Fatalf("regenerate %s: %v", schemaSnapshotPath, err)
		}
		t.Logf("regenerated %s (%d bytes)", schemaSnapshotPath, len(actual))
		return
	}

	expectedBytes, err := os.ReadFile(schemaSnapshotPath)
	if err != nil {
		t.Fatalf("read %s: %v\n\nIf this is the first run, regenerate the snapshot with:\n  %s=1 go test ./internal/store -run %s",
			schemaSnapshotPath, err, regenSchemaSnapshotEnv, t.Name())
	}
	expected := string(expectedBytes)

	if actual == expected {
		return
	}
	t.Fatalf(
		"schema snapshot drift detected.\n\nIf the change is intentional, regenerate with:\n"+
			"  %s=1 go test ./internal/store -run %s\n\n"+
			"--- expected (%s)\n+++ actual (live schema)\n%s",
		regenSchemaSnapshotEnv, t.Name(), schemaSnapshotPath, simpleLineDiff(expected, actual),
	)
}

// simpleLineDiff produces a unified-style line diff sufficient for the
// snapshot drift error message. It is not a full diff implementation —
// just enough to point a developer at the changed lines without pulling
// in a third-party diff package.
func simpleLineDiff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	var b strings.Builder
	maxLen := len(wantLines)
	if len(gotLines) > maxLen {
		maxLen = len(gotLines)
	}
	for i := 0; i < maxLen; i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w == g {
			continue
		}
		if w != "" {
			b.WriteString("- ")
			b.WriteString(w)
			b.WriteString("\n")
		}
		if g != "" {
			b.WriteString("+ ")
			b.WriteString(g)
			b.WriteString("\n")
		}
	}
	return b.String()
}
