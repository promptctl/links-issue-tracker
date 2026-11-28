package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
)

// ReadProjectFile reads the checkout's project config file at path in the two
// versions an apply compares: the working file, and the one the checkout's
// HEAD commit holds. root is the checkout's top directory, which path must lie
// inside. A checkout with no commit holds no committed file.
// [LAW:effects-at-boundaries] the reads; config.StandingOf and
// ProjectFile.Applicable judge them.
func ReadProjectFile(ctx context.Context, env environ.Env, root string, path pathspec.PathSpec) (config.ProjectFile, error) {
	file := config.ProjectFile{Path: path.String()}
	working, err := os.ReadFile(path.String())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return config.ProjectFile{}, fmt.Errorf("read the project config %s: %w", path, err)
	default:
		file.Working = config.FileVersion{Present: true, Content: string(working)}
	}
	rel, err := filepath.Rel(root, path.String())
	if err != nil || !filepath.IsLocal(rel) {
		return config.ProjectFile{}, fmt.Errorf("the project config %s is outside the checkout %s, so no commit of the checkout holds it", path, root)
	}
	// --verify --quiet answers a revision that names nothing, a file HEAD
	// does not hold or a HEAD that does not exist yet, with exit status 1 and
	// no message.
	blob, err := gitOutput(ctx, env, root, "rev-parse", "--verify", "--quiet", "HEAD:"+filepath.ToSlash(rel))
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return file, nil
	}
	if err != nil {
		return config.ProjectFile{}, fmt.Errorf("find the committed project config %s: %w", path, err)
	}
	committed, err := gitStdout(ctx, env, root, "cat-file", "blob", blob)
	if err != nil {
		return config.ProjectFile{}, fmt.Errorf("read the committed project config %s: %w", path, err)
	}
	file.Committed = config.FileVersion{Present: true, Content: committed}
	return file, nil
}
