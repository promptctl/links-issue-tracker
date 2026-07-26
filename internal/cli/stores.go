package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// runStores lists every discovered lit store beneath the given roots, one
// canonical storage directory per line, sorted. With no roots it scans the
// current directory.
//
// [CLI] stdout carries one machine-parseable path per line and nothing else, so
// the output composes into a pipeline; a scan that finds no stores exits 0 with
// empty output rather than reporting an error.
func runStores(stdout io.Writer, args []string) error {
	roots := args
	if len(roots) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get cwd: %w", err)
		}
		roots = []string{cwd}
	}
	locations, err := workspace.Discover(roots)
	if err != nil {
		return err
	}
	for _, loc := range locations {
		if _, err := fmt.Fprintln(stdout, loc.StorageDir); err != nil {
			return err
		}
	}
	return nil
}
