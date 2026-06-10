package cli

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// The identity header is where prefix provenance becomes observable: the one
// run that mints a prefix the user never chose must say so.
func TestPrintWorkspaceIdentityReportsPrefixSource(t *testing.T) {
	var out bytes.Buffer
	ws := workspace.Info{
		StorageDir:   "/tmp/store",
		WorkspaceID:  "ws-id",
		IssuePrefix:  testIssuePrefix(t, "test"),
		GitCommonDir: "/tmp/repo/.git",
	}
	if err := printWorkspaceIdentity(&out, ws); err != nil {
		t.Fatalf("printWorkspaceIdentity() error = %v", err)
	}
	if !strings.Contains(out.String(), "issue_prefix=test issue_prefix_source=configured") {
		t.Fatalf("identity line = %q, want configured provenance", out.String())
	}
}

func TestPrintWorkspaceIdentityReportsDerivedPrefix(t *testing.T) {
	repo := t.TempDir()
	gitInit := exec.Command("git", "init")
	gitInit.Dir = repo
	if output, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	// A derived PrefixSpec is only mintable by the workspace resolver itself —
	// the first resolve of a fresh repo derives the prefix from the repo name.
	ws, err := workspace.Resolve(repo)
	if err != nil {
		t.Fatalf("workspace.Resolve() error = %v", err)
	}
	if !ws.IssuePrefix.Derived() {
		t.Fatalf("first resolve of a fresh repo must derive the prefix")
	}

	var out bytes.Buffer
	if err := printWorkspaceIdentity(&out, ws); err != nil {
		t.Fatalf("printWorkspaceIdentity() error = %v", err)
	}
	if !strings.Contains(out.String(), "issue_prefix_source=derived") {
		t.Fatalf("identity line = %q, want derived provenance", out.String())
	}
}
