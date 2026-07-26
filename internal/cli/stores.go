package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// runStores lists every discovered lit store beneath the given roots, one
// canonical storage directory per line, sorted. With no roots it scans the
// current directory.
//
// [CLI] stdout carries one machine-parseable path per line and nothing else, so
// the output composes into a pipeline; a scan that finds no stores exits 0 with
// empty output rather than reporting an error. Locations stream line by line as
// they are formatted: a write failure (a closed downstream pipe) stops with a
// non-zero exit after the lines already emitted, the standard streaming-CLI
// contract, rather than buffering the whole scan in memory to flush at once.
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
		// [LAW:no-silent-failure] Wrap at the CLI boundary so the failure names
		// the command the operator ran, not just the workspace-layer detail.
		return fmt.Errorf("discover stores: %w", err)
	}
	for _, loc := range locations {
		if _, err := fmt.Fprintln(stdout, loc.StorageDir); err != nil {
			return err
		}
	}
	return nil
}

// runLsAt lists the active issues of the lit store at an explicit storage
// directory — one of the paths `lit stores` prints — WITHOUT depending on the
// current working directory's git repository. It is `lit ls` pointed at a foreign
// store: same active-work default (open + in_progress) and same per-line format,
// differing only in that the store is named by path instead of resolved from cwd.
//
// [LAW:one-source-of-truth] The storage-directory string is turned back into a
// full store Location through LocationFromStorageDir — the one place that geometry
// lives — so `lit stores` output feeds straight in without this command
// re-deriving the "dolt"/"config.json" layout.
//
// [CLI] The store opens strictly read-only via OpenLocationForRead: a concurrent
// writer in the store's own project is unaffected, and no write engine is taken.
// stdout carries the issue lines and nothing else; a store that has no active
// issues exits 0 with empty output.
func runLsAt(ctx context.Context, stdout io.Writer, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: lit ls-at <store-dir>  (a storage directory from `lit stores`)")
	}
	loc := workspace.LocationFromStorageDir(args[0])
	st, err := app.OpenLocationForRead(ctx, loc)
	if err != nil {
		// [LAW:no-silent-failure] Name the path the operator pointed at so a wrong
		// or un-initialized store dir is an actionable error, not an empty list.
		return fmt.Errorf("open store at %q read-only: %w", args[0], err)
	}
	defer func() { _ = st.Close() }()

	// [LAW:one-source-of-truth] Mirror `lit ls`'s active-work default (exclude
	// closed) as data, so "that store's issues" means the same set here as there.
	issues, err := st.ListIssues(ctx, store.ListIssuesFilter{
		Statuses: []model.State{model.StateOpen, model.StateInProgress},
	})
	if err != nil {
		// [LAW:no-silent-failure] Name the target path — as the open error above
		// does — so a read failure over many stores (lit stores | xargs lit ls-at)
		// says which store's read broke, not a bare store-layer error.
		return fmt.Errorf("list issues at %q: %w", args[0], err)
	}
	return printIssueLines(stdout, issues, nil, nil)
}
