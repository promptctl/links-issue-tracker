// [LAW:one-way-deps] these tests sit outside package workspace because they
// build their workspace with workspacetest, which imports workspace.
package workspace_test

import (
	"errors"
	"os"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
	"github.com/promptctl/links-issue-tracker/internal/workspace/workspacetest"
)

// TestFailedEnumerationIsNeverTheNotARepoSentinel keeps a failure to enumerate
// distinguishable from "there is no repository here". ErrNotGitRepo is a repo
// GATE's answer and Discover skips silently on it, so a caller that read it that
// way here would take a failed enumeration for an empty one — and an empty
// enumeration is this machine declaring every checkout of the workspace deleted.
func TestFailedEnumerationIsNeverTheNotARepoSentinel(t *testing.T) {
	env := environ.Of(os.Environ())
	_, err := workspace.LiveCheckouts(env, workspacetest.New(t, env).Checkout(t.TempDir()).Info())
	if err == nil {
		t.Fatal("enumerating outside a repository must fail")
	}
	if errors.Is(err, workspace.ErrNotGitRepo) {
		t.Fatalf("LiveCheckouts() error = %v, which callers may skip on; a failure to enumerate must never read as an empty enumeration", err)
	}
}
