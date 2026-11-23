// Package environgit runs git through the module's Env.Git.
package environgit

import (
	"testing"

	"example.com/fixtures/internal/environ"
)

// Env.Git is a marker: git, and nothing past it.
func TestEnvGitIsGit(t *testing.T) { _ = environ.Env{}.Git("status") }
