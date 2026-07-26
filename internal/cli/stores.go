package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

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

// projectRollup is one project's row in the cross-project overview: either its
// workable counts (the store read cleanly) or the error that made it unreadable.
// A non-nil Err discriminates an error row; StorageDir identifies the project in
// both cases, and Label is its friendlier issue-prefix name when config was read.
// [LAW:types-are-the-program] The presence of Err IS the row kind — the renderer
// dispatches on it once rather than every caller re-asking "did this store work".
type projectRollup struct {
	Label      string // issue prefix from config; falls back to StorageDir
	StorageDir string
	Ready      int
	InFlight   int
	Blocked    int
	Err        error
}

// runOverview renders a holistic cross-project view: it discovers every lit store
// beneath the given roots (default: current directory) and reports each project's
// ready / in-flight / blocked workable counts read-only, with an aggregate across
// them. It is the many-store rollup of the single-store `lit ls-at`.
//
// [LAW:one-source-of-truth] The ready/in-flight/blocked classification is the
// exact partition `lit ready` uses (classifyWorkable + partitionWorkable), so a
// project's cross-project counts can never disagree with its own `lit ready`.
// Per-repo required-fields policy is deliberately NOT applied across the boundary:
// it is repo config, not a store fact, and a discovered Location carries no repo
// root to load it from — so readiness here is store-intrinsic, matching `lit ready`
// exactly for the common case of no configured required_fields.
//
// [CLI] Every store opens strictly read-only, never contending with a project's
// own writer. stdout carries the human-readable table; a store that cannot be read
// is a clearly marked error row, not a fatal error and never silently dropped.
func runOverview(ctx context.Context, stdout io.Writer, args []string) error {
	roots := args
	if len(roots) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get cwd: %w", err)
		}
		roots = []string{cwd}
	}
	rows, err := gatherCrossProjectRollup(ctx, roots)
	if err != nil {
		return err
	}
	return printCrossProjectRollup(stdout, rows)
}

// gatherCrossProjectRollup discovers every lit store beneath roots and rolls each
// up read-only into its workable counts. [LAW:effects-at-boundaries] The discovery
// walk and the per-store opens/reads are the IO; each row's counts are a pure
// partition over that store's already-listed workable leaves. Discovery failure is
// fatal and loud, but a single store that fails to open or read becomes an error
// row [LAW:no-silent-failure] — surfaced, never skipped — so one broken store can
// never hide the projects that read cleanly.
func gatherCrossProjectRollup(ctx context.Context, roots []string) ([]projectRollup, error) {
	locations, err := workspace.Discover(roots)
	if err != nil {
		return nil, fmt.Errorf("discover stores: %w", err)
	}
	rows := make([]projectRollup, 0, len(locations))
	for _, loc := range locations {
		rows = append(rows, rollupLocation(ctx, loc))
	}
	return rows, nil
}

// rollupLocation opens one discovered store read-only and counts its workable
// leaves by readiness. [LAW:no-silent-failure] Any open or read failure is captured
// into the row's Err instead of aborting the overview, so an unreadable store is a
// marked row beside the ones that read.
//
// The issue prefix makes a friendlier label than the long storage path; reading it
// here re-reads config that OpenLocationForRead also reads for the workspace_id —
// cheap read-only JSON with no drift risk, and it keeps the cross-project open on
// the single OpenLocationForRead path rather than opening the store by hand.
func rollupLocation(ctx context.Context, loc workspace.Location) projectRollup {
	row := projectRollup{Label: loc.StorageDir, StorageDir: loc.StorageDir}
	if cfg, err := workspace.ReadConfig(loc.ConfigPath); err == nil && cfg.IssuePrefix != "" {
		row.Label = cfg.IssuePrefix
	}
	st, err := app.OpenLocationForRead(ctx, loc)
	if err != nil {
		row.Err = err
		return row
	}
	defer func() { _ = st.Close() }()
	// nil required-fields: the repo-local needs-design gate is not a store fact and
	// does not cross the store boundary; see runOverview's [LAW:one-source-of-truth].
	annotated, _, err := classifyWorkable(ctx, st, nil, workableFilter{})
	if err != nil {
		row.Err = err
		return row
	}
	inProgress, ready, blocked := partitionWorkable(annotated)
	row.Ready = len(ready)
	row.InFlight = len(inProgress)
	row.Blocked = len(blocked)
	return row
}

// printCrossProjectRollup renders the gathered projects as an aligned table —
// per-project ready / in-flight / blocked counts and a TOTAL row summing the
// projects that read — then any unreadable stores as clearly marked error rows.
// [LAW:effects-at-boundaries] Pure projection over the rows; the only effects are
// the writes. [LAW:no-silent-failure] Error rows print after the table, loud and
// unmissable, never folded into counts they have none of.
func printCrossProjectRollup(w io.Writer, rows []projectRollup) error {
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "PROJECT\tREADY\tIN-FLIGHT\tBLOCKED"); err != nil {
		return err
	}
	var totalReady, totalInFlight, totalBlocked int
	var errored []projectRollup
	for _, row := range rows {
		if row.Err != nil {
			errored = append(errored, row)
			continue
		}
		totalReady += row.Ready
		totalInFlight += row.InFlight
		totalBlocked += row.Blocked
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%d\t%d\n", row.Label, row.Ready, row.InFlight, row.Blocked); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(tw, "TOTAL\t%d\t%d\t%d\n", totalReady, totalInFlight, totalBlocked); err != nil {
		return err
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, row := range errored {
		if _, err := fmt.Fprintf(w, "! %s: %v\n", row.StorageDir, row.Err); err != nil {
			return err
		}
	}
	return nil
}
