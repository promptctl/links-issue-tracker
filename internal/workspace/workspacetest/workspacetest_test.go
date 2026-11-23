package workspacetest_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/suiteenv"
	"github.com/promptctl/links-issue-tracker/internal/workspace/workspacetest"
)

// A built checkout is where Resolve puts the same checkout: every path of its
// geometry, its root, and its being the primary clone. This is the contract
// that lets a test hold the builder's workspace in place of a resolved one.
func TestACheckoutHasTheGeometryResolveGivesIt(t *testing.T) {
	t.Parallel()
	env := suiteenv.Of(os.Environ())
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "--quiet", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", dir, err, out)
	}

	built := workspacetest.Checkout(t, env, dir).Info()
	resolved := workspacetest.Resolved(t, env, dir)

	if built.Location != resolved.Location {
		t.Errorf("built Location = %+v\nresolved Location = %+v", built.Location, resolved.Location)
	}
	if built.RootDir != resolved.RootDir {
		t.Errorf("built RootDir = %q, resolved %q", built.RootDir, resolved.RootDir)
	}
	if built.PrivateGitDir != resolved.PrivateGitDir {
		t.Errorf("built PrivateGitDir = %q, resolved %q", built.PrivateGitDir, resolved.PrivateGitDir)
	}
	if !built.PrimaryClone() || !resolved.PrimaryClone() {
		t.Errorf("PrimaryClone built %t, resolved %t; want both true", built.PrimaryClone(), resolved.PrimaryClone())
	}
	if _, err := os.Stat(built.StorageDir); err != nil {
		t.Errorf("built storage dir: %v; want it made, as Resolve makes it", err)
	}
}
