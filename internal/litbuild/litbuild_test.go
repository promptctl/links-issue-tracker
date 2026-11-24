package litbuild

import (
	"os"
	"path/filepath"
	"testing"
)

// The module root is where ./cmd/lit lives, found from this package's source
// whatever the working directory — this test's own cwd is internal/litbuild.
func TestModuleRootHoldsTheLitModule(t *testing.T) {
	t.Parallel()
	root, err := ModuleRoot()
	if err != nil {
		t.Fatalf("ModuleRoot() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "cmd", "lit", "main.go")); err != nil {
		t.Errorf("ModuleRoot() = %q, which holds no cmd/lit/main.go: %v", root, err)
	}
}
