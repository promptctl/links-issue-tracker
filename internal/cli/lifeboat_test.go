package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// seedWorkspace creates a real current-baseline workspace at the canonical path
// and closes it, so a recover run can dump it below the gate and rebuild it.
func seedWorkspace(t *testing.T) workspace.Info {
	t.Helper()
	root := t.TempDir()
	canonical := filepath.Join(root, "dolt")
	st, err := store.Open(context.Background(), canonical, "test-workspace-id")
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close seed workspace: %v", err)
	}
	return workspace.Info{
		RootDir:      root,
		DatabasePath: canonical,
		WorkspaceID:  "test-workspace-id",
		IssuePrefix:  "test",
	}
}

// TestRunLifeboatRecoverPromotesRecognizedWorkspace is the CLI acceptance for the
// autonomous path: a recognized workspace recovers with no human input, the
// rebuild is promoted in place, and the prior contents are preserved as a backup.
func TestRunLifeboatRecoverPromotesRecognizedWorkspace(t *testing.T) {
	ws := seedWorkspace(t)
	var out bytes.Buffer

	if err := runLifeboatRecover(context.Background(), &out, ws, nil); err != nil {
		t.Fatalf("runLifeboatRecover: %v", err)
	}
	if !strings.Contains(out.String(), "recovered:") {
		t.Fatalf("expected a recovery confirmation, got: %q", out.String())
	}

	// The canonical path still holds a readable workspace, and a backup was kept.
	entries, err := os.ReadDir(ws.RootDir)
	if err != nil {
		t.Fatalf("read storage dir: %v", err)
	}
	var sawDolt, sawBackup bool
	for _, e := range entries {
		if e.Name() == "dolt" {
			sawDolt = true
		}
		if strings.HasPrefix(e.Name(), "dolt.backup-") {
			sawBackup = true
		}
	}
	if !sawDolt || !sawBackup {
		t.Fatalf("want canonical dolt dir and a backup; entries=%v", entries)
	}
}

// TestRunLifeboatRecoverRejectsExtraArgs guards the verb's argument contract.
func TestRunLifeboatRecoverRejectsExtraArgs(t *testing.T) {
	ws := seedWorkspace(t)
	var out bytes.Buffer
	if err := runLifeboatRecover(context.Background(), &out, ws, []string{"unexpected"}); err == nil {
		t.Fatal("expected a usage error for extra arguments")
	}
}

// writeMappingFile encodes a mapping to a temp JSON file and returns its path —
// the artifact an operator hands to `lit lifeboat recover --mapping`.
func writeMappingFile(t *testing.T, m store.ShapeMapping) string {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal mapping: %v", err)
	}
	path := filepath.Join(t.TempDir(), "mapping.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write mapping: %v", err)
	}
	return path
}

// mappingForWorkspace dumps a workspace and proposes its deterministic mapping —
// a known-valid mapping to drive the operator path's wiring without hand-authoring.
func mappingForWorkspace(t *testing.T, ws workspace.Info) store.ShapeMapping {
	t.Helper()
	dump, err := store.DumpRaw(context.Background(), ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("dump workspace: %v", err)
	}
	m, ok := store.DeterministicMap(dump)
	if !ok {
		t.Fatal("deterministic map declined a freshly seeded baseline workspace")
	}
	return m
}

// TestRunLifeboatRecoverWithOperatorMapping is the CLI acceptance for the
// operator path: a mapping supplied as a file drives the identical
// dump→apply→verify→promote pipeline to a promoted, Doctor-clean rebuild. This is
// the door into the recovery engine for any shape the deterministic mapper cannot
// recognize — exercised here through the real flag and file.
func TestRunLifeboatRecoverWithOperatorMapping(t *testing.T) {
	ws := seedWorkspace(t)
	path := writeMappingFile(t, mappingForWorkspace(t, ws))

	var out bytes.Buffer
	if err := runLifeboatRecover(context.Background(), &out, ws, []string{"--mapping", path}); err != nil {
		t.Fatalf("runLifeboatRecover --mapping: %v", err)
	}
	if !strings.Contains(out.String(), "recovered:") {
		t.Fatalf("expected a recovery confirmation, got: %q", out.String())
	}
}

// TestRunLifeboatRecoverJSONModeEmitsMachinePayload locks the --json contract:
// the success path emits a single JSON document and no human text, so a machine
// consumer of `lit lifeboat recover --json` parses one object and nothing else.
func TestRunLifeboatRecoverJSONModeEmitsMachinePayload(t *testing.T) {
	ws := seedWorkspace(t)
	var out bytes.Buffer
	if err := runLifeboatRecover(context.Background(), &out, ws, []string{"--json"}); err != nil {
		t.Fatalf("runLifeboatRecover --json: %v", err)
	}
	dec := json.NewDecoder(&out)
	var got struct {
		Status    string `json:"status"`
		Canonical string `json:"canonical"`
		Backup    string `json:"backup"`
	}
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("output is not a JSON document: %v\nraw: %q", err, out.String())
	}
	if got.Status != "recovered" || got.Canonical != ws.DatabasePath {
		t.Fatalf("unexpected payload: %+v", got)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err == nil {
		t.Fatalf("JSON mode must emit exactly one document, found trailing: %s", trailing)
	}
	if strings.Contains(out.String(), "recovered:") {
		t.Fatalf("JSON mode must not emit human text, got: %q", out.String())
	}
}

// TestRunLifeboatRecoverMissingMappingFile locks the trust boundary: a path that
// does not resolve fails loudly before any workspace mutation.
func TestRunLifeboatRecoverMissingMappingFile(t *testing.T) {
	ws := seedWorkspace(t)
	var out bytes.Buffer
	err := runLifeboatRecover(context.Background(), &out, ws, []string{"--mapping", filepath.Join(t.TempDir(), "absent.json")})
	if err == nil {
		t.Fatal("expected an error for a missing mapping file")
	}
}

// TestRunLifeboatRecoverIncompleteMappingNamesGaps is the operator's convergence
// signal: a mapping missing a source column does not silently drop it — recovery
// fails loudly and the residual names the unaccounted-for column, which is the
// operator's worklist for the next edit-and-rerun.
func TestRunLifeboatRecoverIncompleteMappingNamesGaps(t *testing.T) {
	ws := seedWorkspace(t)
	m := mappingForWorkspace(t, ws)
	removed := store.ColumnRef{Table: "issues", Column: "title"}
	if _, ok := m.Columns[removed]; !ok {
		t.Fatalf("fixture assumption broken: %s not in the deterministic mapping", removed)
	}
	delete(m.Columns, removed)
	path := writeMappingFile(t, m)

	var out bytes.Buffer
	err := runLifeboatRecover(context.Background(), &out, ws, []string{"--mapping", path})
	if err == nil {
		t.Fatal("expected recovery to fail on a non-total mapping")
	}
	if !strings.Contains(err.Error(), "not total") || !strings.Contains(err.Error(), removed.String()) {
		t.Fatalf("residual must name the unaccounted-for column %s, got: %v", removed, err)
	}
}
