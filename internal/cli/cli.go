package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/backup"
	"github.com/bmf/links-issue-tracker/internal/beads"
	"github.com/bmf/links-issue-tracker/internal/doltcli"
	"github.com/bmf/links-issue-tracker/internal/merge"
	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/query"
	"github.com/bmf/links-issue-tracker/internal/store"
	"github.com/bmf/links-issue-tracker/internal/syncfile"
	"github.com/bmf/links-issue-tracker/internal/workspace"
)

func Run(ctx context.Context, stdout io.Writer, stderr io.Writer, args []string) error {
	if len(args) == 0 {
		printUsage(stderr)
		return nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	case "quickstart":
		return runQuickstart(stdout, args[1:])
	case "completion":
		return runCompletion(stdout, args[1:])
	case "hooks":
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get cwd: %w", err)
		}
		ws, err := workspace.Resolve(cwd)
		if err != nil {
			if errors.Is(err, workspace.ErrNotGitRepo) {
				return fmt.Errorf("links requires running inside a git repository/worktree")
			}
			return err
		}
		return runHooks(stdout, ws, args[1:])
	case "sync":
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("get cwd: %w", err)
		}
		ws, err := workspace.Resolve(cwd)
		if err != nil {
			if errors.Is(err, workspace.ErrNotGitRepo) {
				return fmt.Errorf("links requires running inside a git repository/worktree")
			}
			return err
		}
		if _, err := doltcli.RequireMinimumVersion(ctx, ws.RootDir, doltcli.MinSupportedVersion); err != nil {
			return err
		}
		if err := store.EnsureDatabase(ctx, ws.DatabasePath, ws.WorkspaceID); err != nil {
			return err
		}
		return runSync(ctx, stdout, ws, args[1:])
	}
	ap, err := app.OpenFromWD(ctx)
	if err != nil {
		if errors.Is(err, workspace.ErrNotGitRepo) {
			return fmt.Errorf("links requires running inside a git repository/worktree")
		}
		return err
	}
	defer ap.Close()

	switch args[0] {
	case "new":
		return runNew(ctx, stdout, ap, args[1:])
	case "ls":
		return runList(ctx, stdout, ap, args[1:])
	case "show":
		return runShow(ctx, stdout, ap, args[1:])
	case "edit":
		return runEdit(ctx, stdout, ap, args[1:])
	case "close":
		return runTransition(ctx, stdout, ap, args[1:], "close")
	case "open":
		return runTransition(ctx, stdout, ap, args[1:], "reopen")
	case "archive":
		return runTransition(ctx, stdout, ap, args[1:], "archive")
	case "delete":
		return runTransition(ctx, stdout, ap, args[1:], "delete")
	case "unarchive":
		return runTransition(ctx, stdout, ap, args[1:], "unarchive")
	case "restore":
		return runTransition(ctx, stdout, ap, args[1:], "restore")
	case "comment":
		return runComment(ctx, stdout, ap, args[1:])
	case "label":
		return runLabel(ctx, stdout, ap, args[1:])
	case "parent":
		return runParent(ctx, stdout, ap, args[1:])
	case "children":
		return runChildren(ctx, stdout, ap, args[1:])
	case "dep":
		return runDep(ctx, stdout, ap, args[1:])
	case "export":
		return runExport(ctx, stdout, ap, args[1:])
	case "beads":
		return runBeads(ctx, stdout, ap, args[1:])
	case "workspace":
		return runWorkspace(stdout, ap, args[1:])
	case "doctor":
		return runDoctor(ctx, stdout, ap, args[1:])
	case "fsck":
		return runFsck(ctx, stdout, ap, args[1:])
	case "backup":
		return runBackup(ctx, stdout, ap, args[1:])
	case "recover":
		return runRecover(ctx, stdout, ap, args[1:])
	case "bulk":
		return runBulk(ctx, stdout, ap, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runNew(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	title := fs.String("title", "", "Issue title")
	description := fs.String("description", "", "Issue description")
	issueType := fs.String("type", "task", "Issue type: task|feature|bug|chore|epic")
	priority := fs.Int("priority", 2, "Priority 0..4 (lower is more important)")
	assignee := fs.String("assignee", "", "Assignee")
	labels := fs.String("labels", "", "Comma-separated labels")
	expectedRevision := fs.Int64("expected-revision", -1, "Expected workspace revision for optimistic concurrency")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	issue, err := ap.Store.CreateIssue(ctx, store.CreateIssueInput{
		Title: *title, Description: *description, IssueType: *issueType, Priority: *priority, Assignee: *assignee, Labels: splitCSV(*labels), ExpectedRevision: optionalExpectedRevision(fs, expectedRevision),
	})
	if err != nil {
		return err
	}
	return printValue(stdout, issue, *jsonOut, printIssueSummary)
}

func runList(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	status := fs.String("status", "", "Filter by status")
	issueType := fs.String("type", "", "Filter by issue type")
	assignee := fs.String("assignee", "", "Filter by assignee")
	priorityMin := fs.Int("priority-min", -1, "Minimum priority 0..4")
	priorityMax := fs.Int("priority-max", -1, "Maximum priority 0..4")
	search := fs.String("search", "", "Search title and description text")
	ids := fs.String("ids", "", "Comma-separated issue IDs")
	labels := fs.String("labels", "", "Comma-separated labels all of which must match")
	hasComments := fs.Bool("has-comments", false, "Only include issues with comments")
	includeArchived := fs.Bool("include-archived", false, "Include archived issues")
	includeDeleted := fs.Bool("include-deleted", false, "Include deleted issues")
	updatedAfter := fs.String("updated-after", "", "Only include issues updated at or after RFC3339 timestamp")
	updatedBefore := fs.String("updated-before", "", "Only include issues updated at or before RFC3339 timestamp")
	queryExpr := fs.String("query", "", "Query language: status:open type:task priority<=2 has:comments text")
	sortExpr := fs.String("sort", "", "Sort fields, e.g. priority:asc,updated_at:desc")
	columnsExpr := fs.String("columns", "", "Comma-separated output columns")
	format := fs.String("format", "lines", "Output format: lines|table")
	limit := fs.Int("limit", 0, "Limit results")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	filter := store.ListIssuesFilter{
		Status:          strings.TrimSpace(*status),
		IssueType:       strings.TrimSpace(*issueType),
		Assignee:        strings.TrimSpace(*assignee),
		IncludeArchived: *includeArchived,
		IncludeDeleted:  *includeDeleted,
		Limit:           *limit,
	}
	if strings.TrimSpace(*sortExpr) != "" {
		sortSpecs, err := parseSortSpecs(*sortExpr)
		if err != nil {
			return err
		}
		filter.SortBy = sortSpecs
	}
	if visited["priority-min"] {
		value := *priorityMin
		filter.PriorityMin = &value
	}
	if visited["priority-max"] {
		value := *priorityMax
		filter.PriorityMax = &value
	}
	if visited["search"] {
		filter.SearchTerms = append(filter.SearchTerms, strings.TrimSpace(*search))
	}
	if visited["ids"] {
		filter.IDs = splitCSV(*ids)
	}
	if visited["labels"] {
		filter.LabelsAll = splitCSV(*labels)
	}
	if visited["has-comments"] {
		value := *hasComments
		filter.HasComments = &value
	}
	if visited["updated-after"] {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*updatedAfter))
		if err != nil {
			return fmt.Errorf("parse --updated-after: %w", err)
		}
		filter.UpdatedAfter = &parsed
	}
	if visited["updated-before"] {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*updatedBefore))
		if err != nil {
			return fmt.Errorf("parse --updated-before: %w", err)
		}
		filter.UpdatedBefore = &parsed
	}
	if strings.TrimSpace(*queryExpr) != "" {
		parsed, err := query.Parse(*queryExpr)
		if err != nil {
			return err
		}
		filter, err = query.Merge(filter, parsed.Filter)
		if err != nil {
			return err
		}
	}
	issues, err := ap.Store.ListIssues(ctx, filter)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeJSON(stdout, issues)
	}
	columns := parseColumns(*columnsExpr)
	switch strings.ToLower(strings.TrimSpace(*format)) {
	case "", "lines":
		return printIssueLines(stdout, issues, columns)
	case "table":
		return printIssueTable(stdout, issues, columns)
	default:
		return fmt.Errorf("unsupported --format %q", *format)
	}
}

func runShow(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	if len(positional) != 1 {
		return errors.New("usage: lit show <id>")
	}
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit show <id>")
	}
	detail, err := ap.Store.GetIssueDetail(ctx, positional[0])
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeJSON(stdout, detail)
	}
	return printIssueDetail(stdout, detail)
}

func runEdit(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	if len(positional) != 1 {
		return errors.New("usage: lit edit <id> [flags]")
	}
	fs := flag.NewFlagSet("edit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	title := fs.String("title", "", "New title")
	description := fs.String("description", "", "New description")
	issueType := fs.String("type", "", "New issue type")
	priority := fs.Int("priority", -1, "New priority")
	assignee := fs.String("assignee", "", "New assignee")
	labels := fs.String("labels", "", "Replace labels with a comma-separated set")
	clearAssignee := fs.Bool("clear-assignee", false, "Clear assignee")
	clearLabels := fs.Bool("clear-labels", false, "Remove all labels")
	expectedRevision := fs.Int64("expected-revision", -1, "Expected workspace revision for optimistic concurrency")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit edit <id> [flags]")
	}
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	var in store.UpdateIssueInput
	if visited["title"] {
		in.Title = title
	}
	if visited["description"] {
		in.Description = description
	}
	if visited["type"] {
		in.IssueType = issueType
	}
	if visited["priority"] {
		in.Priority = priority
	}
	if *clearAssignee {
		empty := ""
		in.Assignee = &empty
	} else if visited["assignee"] {
		in.Assignee = assignee
	}
	if *clearLabels {
		empty := []string{}
		in.Labels = &empty
	} else if visited["labels"] {
		parsed := splitCSV(*labels)
		in.Labels = &parsed
	}
	in.ExpectedRevision = optionalExpectedRevision(fs, expectedRevision)
	issue, err := ap.Store.UpdateIssue(ctx, positional[0], in)
	if err != nil {
		return err
	}
	return printValue(stdout, issue, *jsonOut, printIssueSummary)
}

func runTransition(ctx context.Context, stdout io.Writer, ap *app.App, args []string, action string) error {
	positional, flagArgs := splitArgs(args, 1)
	if len(positional) != 1 {
		return fmt.Errorf("usage: lit %s <id> --reason <text>", transitionCommandName(action))
	}
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reason := fs.String("reason", "", "Lifecycle transition reason")
	by := fs.String("by", os.Getenv("USER"), "Lifecycle actor")
	expectedRevision := fs.Int64("expected-revision", -1, "Expected workspace revision for optimistic concurrency")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: lit %s <id> --reason <text>", transitionCommandName(action))
	}
	issue, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
		IssueID:          positional[0],
		Action:           action,
		Reason:           *reason,
		CreatedBy:        *by,
		ExpectedRevision: optionalExpectedRevision(fs, expectedRevision),
	})
	if err != nil {
		return err
	}
	return printValue(stdout, issue, *jsonOut, printIssueSummary)
}

func runComment(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New("usage: lit comment add <id> --body <text>")
	}
	positional, flagArgs := splitArgs(args[1:], 1)
	if len(positional) != 1 {
		return errors.New("usage: lit comment add <id> --body <text>")
	}
	fs := flag.NewFlagSet("comment add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	body := fs.String("body", "", "Comment body")
	by := fs.String("by", os.Getenv("USER"), "Comment author")
	expectedRevision := fs.Int64("expected-revision", -1, "Expected workspace revision for optimistic concurrency")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit comment add <id> --body <text>")
	}
	comment, err := ap.Store.AddComment(ctx, store.AddCommentInput{IssueID: positional[0], Body: *body, CreatedBy: *by, ExpectedRevision: optionalExpectedRevision(fs, expectedRevision)})
	if err != nil {
		return err
	}
	return printValue(stdout, comment, *jsonOut, func(w io.Writer, v any) error {
		c := v.(model.Comment)
		_, err := fmt.Fprintf(w, "%s %s\n", c.IssueID, c.ID)
		return err
	})
}

func runDep(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit dep <add|rm> ...")
	}
	switch args[0] {
	case "add":
		positional, flagArgs := splitArgs(args[1:], 2)
		if len(positional) != 2 {
			return errors.New("usage: lit dep add <src-id> <dst-id> [--type blocks|parent-child|related-to]")
		}
		fs := flag.NewFlagSet("dep add", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		relType := fs.String("type", "blocks", "Relation type: blocks|parent-child|related-to")
		by := fs.String("by", os.Getenv("USER"), "Relation creator")
		expectedRevision := fs.Int64("expected-revision", -1, "Expected workspace revision for optimistic concurrency")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit dep add <src-id> <dst-id> [--type blocks|parent-child|related-to]")
		}
		rel, err := ap.Store.AddRelation(ctx, store.AddRelationInput{SrcID: positional[0], DstID: positional[1], Type: *relType, CreatedBy: *by, ExpectedRevision: optionalExpectedRevision(fs, expectedRevision)})
		if err != nil {
			return err
		}
		return printValue(stdout, rel, *jsonOut, func(w io.Writer, v any) error {
			r := v.(model.Relation)
			_, err := fmt.Fprintf(w, "%s --%s--> %s\n", r.SrcID, r.Type, r.DstID)
			return err
		})
	case "rm":
		positional, flagArgs := splitArgs(args[1:], 2)
		if len(positional) != 2 {
			return errors.New("usage: lit dep rm <src-id> <dst-id> [--type ...]")
		}
		fs := flag.NewFlagSet("dep rm", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		relType := fs.String("type", "blocks", "Relation type: blocks|parent-child|related-to")
		expectedRevision := fs.Int64("expected-revision", -1, "Expected workspace revision for optimistic concurrency")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit dep rm <src-id> <dst-id> [--type ...]")
		}
		if err := ap.Store.RemoveRelation(ctx, positional[0], positional[1], *relType, optionalExpectedRevision(fs, expectedRevision)); err != nil {
			return err
		}
		return printValue(stdout, map[string]string{"status": "ok"}, *jsonOut, func(w io.Writer, _ any) error {
			_, err := fmt.Fprintln(w, "ok")
			return err
		})
	case "ls":
		positional, flagArgs := splitArgs(args[1:], 1)
		if len(positional) != 1 {
			return errors.New("usage: lit dep ls <issue-id> [--type blocks|parent-child|related-to] [--json]")
		}
		fs := flag.NewFlagSet("dep ls", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		relType := fs.String("type", "", "Filter relation type")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit dep ls <issue-id> [--type blocks|parent-child|related-to] [--json]")
		}
		relations, err := ap.Store.ListRelationsForIssue(ctx, positional[0], *relType)
		if err != nil {
			return err
		}
		return printValue(stdout, relations, *jsonOut, func(w io.Writer, v any) error {
			list := v.([]model.Relation)
			for _, rel := range list {
				if _, err := fmt.Fprintf(w, "%s --%s--> %s\n", rel.SrcID, rel.Type, rel.DstID); err != nil {
					return err
				}
			}
			return nil
		})
	default:
		return errors.New("usage: lit dep <add|rm|ls> ...")
	}
}

func runLabel(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit label <add|rm> ...")
	}
	switch args[0] {
	case "add":
		positional, flagArgs := splitArgs(args[1:], 2)
		if len(positional) != 2 {
			return errors.New("usage: lit label add <issue-id> <label> [--by <user>] [--json]")
		}
		fs := flag.NewFlagSet("label add", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		by := fs.String("by", os.Getenv("USER"), "Label author")
		expectedRevision := fs.Int64("expected-revision", -1, "Expected workspace revision for optimistic concurrency")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit label add <issue-id> <label> [--by <user>] [--json]")
		}
		labels, err := ap.Store.AddLabel(ctx, store.AddLabelInput{IssueID: positional[0], Name: positional[1], CreatedBy: *by, ExpectedRevision: optionalExpectedRevision(fs, expectedRevision)})
		if err != nil {
			return err
		}
		return printValue(stdout, labels, *jsonOut, printLabels)
	case "rm":
		positional, flagArgs := splitArgs(args[1:], 2)
		if len(positional) != 2 {
			return errors.New("usage: lit label rm <issue-id> <label> [--json]")
		}
		fs := flag.NewFlagSet("label rm", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		expectedRevision := fs.Int64("expected-revision", -1, "Expected workspace revision for optimistic concurrency")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: lit label rm <issue-id> <label> [--json]")
		}
		labels, err := ap.Store.RemoveLabel(ctx, positional[0], positional[1], optionalExpectedRevision(fs, expectedRevision))
		if err != nil {
			return err
		}
		return printValue(stdout, labels, *jsonOut, printLabels)
	default:
		return errors.New("usage: lit label <add|rm> ...")
	}
}

func runParent(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit parent <set|clear> ...")
	}
	switch args[0] {
	case "set":
		positional, flagArgs := splitArgs(args[1:], 2)
		if len(positional) != 2 {
			return errors.New("usage: lit parent set <child-id> <parent-id> [--by <user>] [--expected-revision N] [--json]")
		}
		fs := flag.NewFlagSet("parent set", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		by := fs.String("by", os.Getenv("USER"), "Relation creator")
		expectedRevision := fs.Int64("expected-revision", -1, "Expected workspace revision for optimistic concurrency")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		rel, err := ap.Store.SetParent(ctx, store.SetParentInput{
			ChildID:          positional[0],
			ParentID:         positional[1],
			CreatedBy:        *by,
			ExpectedRevision: optionalExpectedRevision(fs, expectedRevision),
		})
		if err != nil {
			return err
		}
		return printValue(stdout, rel, *jsonOut, func(w io.Writer, v any) error {
			relation := v.(model.Relation)
			_, err := fmt.Fprintf(w, "%s --parent-child--> %s\n", relation.SrcID, relation.DstID)
			return err
		})
	case "clear":
		positional, flagArgs := splitArgs(args[1:], 1)
		if len(positional) != 1 {
			return errors.New("usage: lit parent clear <child-id> [--expected-revision N] [--json]")
		}
		fs := flag.NewFlagSet("parent clear", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		expectedRevision := fs.Int64("expected-revision", -1, "Expected workspace revision for optimistic concurrency")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		if err := ap.Store.ClearParent(ctx, positional[0], optionalExpectedRevision(fs, expectedRevision)); err != nil {
			return err
		}
		return printValue(stdout, map[string]string{"status": "ok"}, *jsonOut, func(w io.Writer, _ any) error {
			_, err := fmt.Fprintln(w, "ok")
			return err
		})
	default:
		return errors.New("usage: lit parent <set|clear> ...")
	}
}

func runChildren(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	positional, flagArgs := splitArgs(args, 1)
	if len(positional) != 1 {
		return errors.New("usage: lit children <parent-id> [--json]")
	}
	fs := flag.NewFlagSet("children", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	children, err := ap.Store.ListChildren(ctx, positional[0])
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeJSON(stdout, children)
	}
	return printIssueLines(stdout, children, []string{"id", "state", "title"})
}

func runExport(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", true, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	export, err := ap.Store.Export(ctx)
	if err != nil {
		return err
	}
	return printValue(stdout, export, *jsonOut, func(w io.Writer, _ any) error {
		return writeJSON(w, export)
	})
}

func runBeads(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit beads <import|export> --db <path> [--json]")
	}
	switch args[0] {
	case "import":
		fs := flag.NewFlagSet("beads import", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		dbPath := fs.String("db", "", "Path to beads Dolt root/database")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if strings.TrimSpace(*dbPath) == "" {
			return errors.New("usage: lit beads import --db <path> [--json]")
		}
		summary, err := beads.Import(ctx, ap.Store, *dbPath)
		if err != nil {
			return err
		}
		return printValue(stdout, summary, *jsonOut, func(w io.Writer, v any) error {
			s := v.(beads.Summary)
			_, err := fmt.Fprintf(w, "imported issues=%d relations=%d comments=%d labels=%d\n", s.Issues, s.Relations, s.Comments, s.Labels)
			return err
		})
	case "export":
		fs := flag.NewFlagSet("beads export", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		dbPath := fs.String("db", "", "Path to beads Dolt root/database")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if strings.TrimSpace(*dbPath) == "" {
			return errors.New("usage: lit beads export --db <path> [--json]")
		}
		summary, err := beads.Export(ctx, ap.Store, *dbPath)
		if err != nil {
			return err
		}
		return printValue(stdout, summary, *jsonOut, func(w io.Writer, v any) error {
			s := v.(beads.Summary)
			_, err := fmt.Fprintf(w, "exported issues=%d relations=%d comments=%d labels=%d\n", s.Issues, s.Relations, s.Comments, s.Labels)
			return err
		})
	default:
		return errors.New("usage: lit beads <import|export> --db <path> [--json]")
	}
}

func runWorkspace(stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("workspace", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	payload := map[string]string{
		"workspace_id":   ap.Workspace.WorkspaceID,
		"git_common_dir": ap.Workspace.GitCommonDir,
		"storage_dir":    ap.Workspace.StorageDir,
		"database_path":  ap.Workspace.DatabasePath,
		"dolt_repo_path": ap.Workspace.DoltRepoPath,
	}
	if revision, err := ap.Store.GetWorkspaceRevision(context.Background()); err == nil {
		payload["workspace_revision"] = strconv.FormatInt(revision, 10)
	}
	if *jsonOut {
		return writeJSON(stdout, payload)
	}
	for _, key := range []string{"workspace_id", "workspace_revision", "git_common_dir", "storage_dir", "database_path", "dolt_repo_path"} {
		if _, err := fmt.Fprintf(stdout, "%s: %s\n", key, payload[key]); err != nil {
			return err
		}
	}
	return nil
}

func runSync(ctx context.Context, stdout io.Writer, ws workspace.Info, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit sync <status|remote|fetch|pull|push> ...")
	}
	syncState, err := syncDoltRemotesFromGit(ctx, ws)
	if err != nil {
		return err
	}
	switch args[0] {
	case "remote":
		if len(args) < 2 {
			return errors.New("usage: lit sync remote ls [--json]")
		}
		switch args[1] {
		case "ls":
			fs := flag.NewFlagSet("sync remote ls", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			jsonOut := fs.Bool("json", false, "Output JSON")
			if err := fs.Parse(args[2:]); err != nil {
				return err
			}
			payload := map[string]any{
				"git_remotes":  syncState.gitRemotes,
				"dolt_remotes": syncState.doltRemotes,
				"changes":      syncState.changes,
			}
			return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
				p := v.(map[string]any)
				_, err := fmt.Fprintf(
					w,
					"git=%d dolt=%d added=%d updated=%d removed=%d\n",
					len(p["git_remotes"].([]workspace.GitRemote)),
					len(p["dolt_remotes"].([]map[string]string)),
					len(syncState.changes.Added),
					len(syncState.changes.Updated),
					len(syncState.changes.Removed),
				)
				return err
			})
		default:
			return errors.New("usage: lit sync remote ls [--json]")
		}
	case "fetch":
		fs := flag.NewFlagSet("sync fetch", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		remote := fs.String("remote", "origin", "Remote name")
		prune := fs.Bool("prune", false, "Pass --prune to dolt fetch")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		commandArgs := []string{"fetch", strings.TrimSpace(*remote)}
		if *prune {
			commandArgs = append(commandArgs, "--prune")
		}
		output, err := doltcli.Run(ctx, ws.DoltRepoPath, commandArgs...)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"status": "ok",
			"remote": strings.TrimSpace(*remote),
			"raw":    output,
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]any)
			if strings.TrimSpace(p["raw"].(string)) != "" {
				_, err := fmt.Fprintln(w, strings.TrimSpace(p["raw"].(string)))
				return err
			}
			_, err := fmt.Fprintf(w, "fetched %s\n", p["remote"])
			return err
		})
	case "pull":
		fs := flag.NewFlagSet("sync pull", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		remote := fs.String("remote", "origin", "Remote name")
		branch := fs.String("branch", "", "Branch name (defaults to current)")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		commandArgs := []string{"pull", strings.TrimSpace(*remote)}
		if strings.TrimSpace(*branch) != "" {
			commandArgs = append(commandArgs, strings.TrimSpace(*branch))
		}
		output, err := doltcli.Run(ctx, ws.DoltRepoPath, commandArgs...)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"status": "ok",
			"remote": strings.TrimSpace(*remote),
			"branch": strings.TrimSpace(*branch),
			"raw":    output,
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]any)
			if strings.TrimSpace(p["raw"].(string)) != "" {
				_, err := fmt.Fprintln(w, strings.TrimSpace(p["raw"].(string)))
				return err
			}
			if strings.TrimSpace(p["branch"].(string)) != "" {
				_, err := fmt.Fprintf(w, "pulled %s/%s\n", p["remote"], p["branch"])
				return err
			}
			_, err := fmt.Fprintf(w, "pulled %s\n", p["remote"])
			return err
		})
	case "push":
		fs := flag.NewFlagSet("sync push", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		remote := fs.String("remote", "origin", "Remote name")
		branch := fs.String("branch", "", "Branch name (defaults to current)")
		setUpstream := fs.Bool("set-upstream", false, "Pass -u to dolt push")
		force := fs.Bool("force", false, "Pass --force to dolt push")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		commandArgs := []string{"push"}
		if *setUpstream {
			commandArgs = append(commandArgs, "-u")
		}
		if *force {
			commandArgs = append(commandArgs, "--force")
		}
		commandArgs = append(commandArgs, strings.TrimSpace(*remote))
		if strings.TrimSpace(*branch) != "" {
			commandArgs = append(commandArgs, strings.TrimSpace(*branch))
		}
		output, err := doltcli.Run(ctx, ws.DoltRepoPath, commandArgs...)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"status": "ok",
			"remote": strings.TrimSpace(*remote),
			"branch": strings.TrimSpace(*branch),
			"raw":    output,
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]any)
			if strings.TrimSpace(p["raw"].(string)) != "" {
				_, err := fmt.Fprintln(w, strings.TrimSpace(p["raw"].(string)))
				return err
			}
			if strings.TrimSpace(p["branch"].(string)) != "" {
				_, err := fmt.Fprintf(w, "pushed %s/%s\n", p["remote"], p["branch"])
				return err
			}
			_, err := fmt.Fprintf(w, "pushed %s\n", p["remote"])
			return err
		})
	case "status":
		fs := flag.NewFlagSet("sync status", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		version, err := doltcli.InstalledVersion(ctx, ws.DoltRepoPath)
		if err != nil {
			return err
		}
		branch, err := doltcli.Run(ctx, ws.DoltRepoPath, "branch", "--show-current")
		if err != nil {
			return err
		}
		remotesOutput, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "-v")
		if err != nil {
			return err
		}
		statusOutput, err := doltcli.Run(ctx, ws.DoltRepoPath, "status")
		if err != nil {
			return err
		}
		logOutput, err := doltcli.Run(ctx, ws.DoltRepoPath, "log", "-n", "1", "--oneline")
		if err != nil {
			return err
		}
		payload := map[string]any{
			"dolt_version": version.String(),
			"branch":       strings.TrimSpace(branch),
			"head":         strings.TrimSpace(logOutput),
			"status":       strings.TrimSpace(statusOutput),
			"git_remotes":  syncState.gitRemotes,
			"dolt_remotes": parseDoltRemoteVerbose(remotesOutput),
			"changes":      syncState.changes,
		}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]any)
			_, err := fmt.Fprintf(
				w,
				"version=%v branch=%v head=%v git=%d dolt=%d added=%d updated=%d removed=%d\n",
				p["dolt_version"],
				p["branch"],
				p["head"],
				len(p["git_remotes"].([]workspace.GitRemote)),
				len(p["dolt_remotes"].([]map[string]string)),
				len(syncState.changes.Added),
				len(syncState.changes.Updated),
				len(syncState.changes.Removed),
			)
			return err
		})
	default:
		return errors.New("usage: lit sync <status|remote|fetch|pull|push> ...")
	}
}

type remoteSyncChanges struct {
	Added   []string `json:"added"`
	Updated []string `json:"updated"`
	Removed []string `json:"removed"`
}

type remoteSyncState struct {
	gitRemotes  []workspace.GitRemote
	doltRemotes []map[string]string
	changes     remoteSyncChanges
}

func syncDoltRemotesFromGit(ctx context.Context, ws workspace.Info) (remoteSyncState, error) {
	gitRemotes, err := workspace.GitRemotes(ws.RootDir)
	if err != nil {
		return remoteSyncState{}, fmt.Errorf("read git remotes: %w", err)
	}
	doltOutput, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "-v")
	if err != nil {
		return remoteSyncState{}, err
	}
	doltRemotes := parseDoltRemoteVerbose(doltOutput)
	gitByName := mapGitRemotesByName(gitRemotes)
	doltByName := mapRemotesByName(doltRemotes)

	changes := remoteSyncChanges{
		Added:   []string{},
		Updated: []string{},
		Removed: []string{},
	}

	for _, remote := range gitRemotes {
		currentURL, exists := doltByName[remote.Name]
		if !exists {
			if _, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "add", remote.Name, remote.URL); err != nil {
				return remoteSyncState{}, err
			}
			changes.Added = append(changes.Added, remote.Name)
			continue
		}
		if !sameRemoteURL(currentURL, remote.URL) {
			if _, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "remove", remote.Name); err != nil {
				return remoteSyncState{}, err
			}
			if _, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "add", remote.Name, remote.URL); err != nil {
				return remoteSyncState{}, err
			}
			changes.Updated = append(changes.Updated, remote.Name)
		}
	}
	for name := range doltByName {
		if _, keep := gitByName[name]; keep {
			continue
		}
		if _, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "remove", name); err != nil {
			return remoteSyncState{}, err
		}
		changes.Removed = append(changes.Removed, name)
	}
	sort.Strings(changes.Added)
	sort.Strings(changes.Updated)
	sort.Strings(changes.Removed)

	finalOutput, err := doltcli.Run(ctx, ws.DoltRepoPath, "remote", "-v")
	if err != nil {
		return remoteSyncState{}, err
	}
	return remoteSyncState{
		gitRemotes:  gitRemotes,
		doltRemotes: parseDoltRemoteVerbose(finalOutput),
		changes:     changes,
	}, nil
}

func mapGitRemotesByName(remotes []workspace.GitRemote) map[string]string {
	out := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		out[remote.Name] = remote.URL
	}
	return out
}

func mapRemotesByName(remotes []map[string]string) map[string]string {
	out := make(map[string]string, len(remotes))
	for _, remote := range remotes {
		name := strings.TrimSpace(remote["name"])
		url := strings.TrimSpace(remote["url"])
		scope := strings.TrimSpace(remote["scope"])
		if name == "" || url == "" {
			continue
		}
		// [LAW:one-source-of-truth] Remote URL projection always prefers fetch scope as the canonical source.
		if scope == "fetch" {
			out[name] = url
			continue
		}
		if _, exists := out[name]; !exists {
			out[name] = url
		}
	}
	return out
}

func sameRemoteURL(left, right string) bool {
	return normalizeRemoteURL(left) == normalizeRemoteURL(right)
}

func normalizeRemoteURL(input string) string {
	trimmed := strings.TrimSpace(input)
	trimmed = strings.TrimPrefix(trimmed, "git+")
	return trimmed
}

func parseDoltRemoteVerbose(output string) []map[string]string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	remotes := make([]map[string]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			remotes = append(remotes, map[string]string{"raw": trimmed})
			continue
		}
		entry := map[string]string{
			"name": fields[0],
			"url":  fields[1],
		}
		if len(fields) >= 3 {
			entry["scope"] = strings.Trim(fields[2], "()")
		}
		remotes = append(remotes, entry)
	}
	return remotes
}

func runDoctor(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := ap.Store.Doctor(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		if err := writeJSON(stdout, report); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(stdout, "integrity_check=%s foreign_key_issues=%d invalid_related_rows=%d orphan_history_rows=%d\n", report.IntegrityCheck, report.ForeignKeyIssues, report.InvalidRelatedRows, report.OrphanHistoryRows); err != nil {
			return err
		}
	}
	// [LAW:single-enforcer] Corruption classification is output-format agnostic and always enforced here.
	if len(report.Errors) > 0 {
		return CorruptionError{Message: strings.Join(report.Errors, "; ")}
	}
	return nil
}

func runFsck(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("fsck", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repair := fs.Bool("repair", false, "Attempt safe repairs")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := ap.Store.Fsck(ctx, *repair)
	if err != nil {
		return err
	}
	if *jsonOut {
		if err := writeJSON(stdout, report); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(stdout, "integrity_check=%s foreign_key_issues=%d invalid_related_rows=%d orphan_history_rows=%d repair=%t\n", report.IntegrityCheck, report.ForeignKeyIssues, report.InvalidRelatedRows, report.OrphanHistoryRows, *repair); err != nil {
			return err
		}
	}
	// [LAW:single-enforcer] Corruption classification is output-format agnostic and always enforced here.
	if len(report.Errors) > 0 {
		return CorruptionError{Message: strings.Join(report.Errors, "; ")}
	}
	return nil
}

func runBackup(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit backup <create|list|restore> ...")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("backup create", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		keep := fs.Int("keep", 20, "Snapshots to keep after rotation")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		export, err := ap.Store.Export(ctx)
		if err != nil {
			return err
		}
		snapshot, err := backup.Create(ap.Workspace.StorageDir, export)
		if err != nil {
			return err
		}
		if err := backup.Prune(ap.Workspace.StorageDir, *keep); err != nil {
			return err
		}
		return printValue(stdout, snapshot, *jsonOut, func(w io.Writer, v any) error {
			s := v.(backup.Snapshot)
			_, err := fmt.Fprintf(w, "%s %s\n", s.Name, s.Path)
			return err
		})
	case "list":
		fs := flag.NewFlagSet("backup list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		snapshots, err := backup.List(ap.Workspace.StorageDir)
		if err != nil {
			return err
		}
		return printValue(stdout, snapshots, *jsonOut, func(w io.Writer, v any) error {
			list := v.([]backup.Snapshot)
			for _, snapshot := range list {
				if _, err := fmt.Fprintf(w, "%s %d %s\n", snapshot.Name, snapshot.Size, snapshot.Path); err != nil {
					return err
				}
			}
			return nil
		})
	case "restore":
		fs := flag.NewFlagSet("backup restore", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		path := fs.String("path", "", "Backup snapshot path")
		latest := fs.Bool("latest", false, "Restore latest backup snapshot")
		force := fs.Bool("force", false, "Force restore over unsynced state")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		restorePath := strings.TrimSpace(*path)
		if *latest {
			latestSnapshot, err := backup.Latest(ap.Workspace.StorageDir)
			if err != nil {
				return err
			}
			if latestSnapshot == nil {
				return errors.New("no backups available")
			}
			restorePath = latestSnapshot.Path
		}
		if restorePath == "" {
			return errors.New("usage: lit backup restore --path <snapshot.json> [--force] [--json] or --latest")
		}
		if err := restoreFromExportPath(ctx, ap, restorePath, *force); err != nil {
			return err
		}
		payload := map[string]string{"status": "restored", "path": restorePath}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]string)
			_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
			return err
		})
	default:
		return errors.New("usage: lit backup <create|list|restore> ...")
	}
}

func runRecover(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fromSync := fs.String("from-sync", "", "Restore from sync file")
	fromBackup := fs.String("from-backup", "", "Restore from backup snapshot")
	latestBackup := fs.Bool("latest-backup", false, "Restore from latest backup snapshot")
	force := fs.Bool("force", false, "Force restore over unsynced state")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var restorePath string
	switch {
	case strings.TrimSpace(*fromSync) != "":
		restorePath = strings.TrimSpace(*fromSync)
	case strings.TrimSpace(*fromBackup) != "":
		restorePath = strings.TrimSpace(*fromBackup)
	case *latestBackup:
		latest, err := backup.Latest(ap.Workspace.StorageDir)
		if err != nil {
			return err
		}
		if latest == nil {
			return errors.New("no backups available")
		}
		restorePath = latest.Path
	default:
		return errors.New("usage: lit recover --from-sync <path> | --from-backup <path> | --latest-backup [--force] [--json]")
	}
	if err := restoreFromExportPath(ctx, ap, restorePath, *force); err != nil {
		return err
	}
	payload := map[string]string{"status": "recovered", "path": restorePath}
	return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
		p := v.(map[string]string)
		_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
		return err
	})
}

func runBulk(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lit bulk <label|close|archive|import> ...")
	}
	switch args[0] {
	case "label":
		if len(args) < 2 {
			return errors.New("usage: lit bulk label <add|rm> ...")
		}
		action := args[1]
		fs := flag.NewFlagSet("bulk label", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		ids := fs.String("ids", "", "Comma-separated issue IDs")
		label := fs.String("label", "", "Label name")
		by := fs.String("by", os.Getenv("USER"), "Label actor")
		expectedRevision := fs.Int64("expected-revision", -1, "Expected workspace revision for optimistic concurrency")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		issueIDs := splitCSV(*ids)
		if len(issueIDs) == 0 {
			return errors.New("--ids is required")
		}
		if strings.TrimSpace(*label) == "" {
			return errors.New("--label is required")
		}
		results := map[string]string{}
		nextExpected := optionalExpectedRevision(fs, expectedRevision)
		for _, issueID := range issueIDs {
			switch action {
			case "add":
				_, err := ap.Store.AddLabel(ctx, store.AddLabelInput{
					IssueID:          issueID,
					Name:             *label,
					CreatedBy:        *by,
					ExpectedRevision: nextExpected,
				})
				if err != nil {
					results[issueID] = err.Error()
					continue
				}
			case "rm":
				_, err := ap.Store.RemoveLabel(ctx, issueID, *label, nextExpected)
				if err != nil {
					results[issueID] = err.Error()
					continue
				}
			default:
				return errors.New("usage: lit bulk label <add|rm> ...")
			}
			results[issueID] = "ok"
			if nextExpected != nil {
				*nextExpected = *nextExpected + 1
			}
		}
		return printValue(stdout, results, *jsonOut, func(w io.Writer, v any) error {
			entries := v.(map[string]string)
			for issueID, status := range entries {
				if _, err := fmt.Fprintf(w, "%s %s\n", issueID, status); err != nil {
					return err
				}
			}
			return nil
		})
	case "close", "archive":
		fs := flag.NewFlagSet("bulk transition", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		ids := fs.String("ids", "", "Comma-separated issue IDs")
		reason := fs.String("reason", "", "Lifecycle reason")
		by := fs.String("by", os.Getenv("USER"), "Lifecycle actor")
		expectedRevision := fs.Int64("expected-revision", -1, "Expected workspace revision for optimistic concurrency")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		issueIDs := splitCSV(*ids)
		if len(issueIDs) == 0 {
			return errors.New("--ids is required")
		}
		if strings.TrimSpace(*reason) == "" {
			return errors.New("--reason is required")
		}
		results := map[string]string{}
		nextExpected := optionalExpectedRevision(fs, expectedRevision)
		for _, issueID := range issueIDs {
			_, err := ap.Store.TransitionIssue(ctx, store.TransitionIssueInput{
				IssueID:          issueID,
				Action:           args[0],
				Reason:           *reason,
				CreatedBy:        *by,
				ExpectedRevision: nextExpected,
			})
			if err != nil {
				results[issueID] = err.Error()
				continue
			}
			results[issueID] = "ok"
			if nextExpected != nil {
				*nextExpected = *nextExpected + 1
			}
		}
		return printValue(stdout, results, *jsonOut, func(w io.Writer, v any) error {
			entries := v.(map[string]string)
			for issueID, status := range entries {
				if _, err := fmt.Fprintf(w, "%s %s\n", issueID, status); err != nil {
					return err
				}
			}
			return nil
		})
	case "import":
		fs := flag.NewFlagSet("bulk import", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		path := fs.String("path", "", "Path to JSON export")
		force := fs.Bool("force", false, "Force import over unsynced local state")
		jsonOut := fs.Bool("json", false, "Output JSON")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if strings.TrimSpace(*path) == "" {
			return errors.New("--path is required")
		}
		if err := restoreFromExportPath(ctx, ap, *path, *force); err != nil {
			return err
		}
		payload := map[string]string{"status": "imported", "path": filepath.Clean(*path)}
		return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
			p := v.(map[string]string)
			_, err := fmt.Fprintf(w, "%s %s\n", p["status"], p["path"])
			return err
		})
	default:
		return errors.New("usage: lit bulk <label|close|archive|import> ...")
	}
}

func runCompletion(stdout io.Writer, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: lit completion <bash|zsh|fish>")
	}
	switch args[0] {
	case "bash":
		_, err := io.WriteString(stdout, bashCompletionScript)
		return err
	case "zsh":
		_, err := io.WriteString(stdout, zshCompletionScript)
		return err
	case "fish":
		_, err := io.WriteString(stdout, fishCompletionScript)
		return err
	default:
		return errors.New("usage: lit completion <bash|zsh|fish>")
	}
}

func runQuickstart(stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("quickstart", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: lit quickstart [--json]")
	}

	payload := map[string]any{
		"summary": "Agent quickstart for links issue tracking",
		"workflow": []string{
			"Discover workspace identity and revision with `lit workspace --json`.",
			"Install git hook automation once with `lit hooks install`.",
			"List active issues with `lit ls --format lines --json` or narrow with `--query`.",
			"Create issues with `lit new ...`; use `--type epic` for epics.",
			"Connect issues using `lit parent set` and `lit dep add --type related-to|blocks`.",
			"Mutate safely with `--expected-revision` from `workspace_revision`.",
			"Configure remotes with `git remote`; `lit sync` mirrors those remotes into Dolt automatically.",
			"Run health checks with `lit doctor` and repair known corruption with `lit fsck --repair`.",
			"Snapshot and rollback using `lit backup create`, `lit backup restore`, or `lit recover`.",
		},
		"examples": []string{
			"lit hooks install --json",
			"lit workspace --json",
			"lit ls --query \"status:open type:task\" --sort priority:asc,updated_at:desc --json",
			"lit new --title \"Fix renderer race\" --type bug --priority 1 --labels renderer,urgent --json",
			"lit parent set <issue-id> <parent-issue-id> --json",
			"lit dep add <issue-id> <dependency-issue-id> --type related-to --json",
			"lit edit <issue-id> --priority 1 --expected-revision <workspace-revision> --json",
			"git remote add origin https://github.com/org/repo.git",
			"lit sync remote ls --json",
			"lit sync pull --remote origin --branch main --json",
			"lit sync push --remote origin --branch main --json",
		},
		"exit_codes": map[string]int{
			"ok":             ExitOK,
			"usage":          ExitUsage,
			"validation":     ExitValidation,
			"not_found":      ExitNotFound,
			"conflict":       ExitConflict,
			"stale_revision": ExitStaleRevision,
			"corruption":     ExitCorruption,
		},
	}

	return printValue(stdout, payload, *jsonOut, func(w io.Writer, v any) error {
		instructions := v.(map[string]any)
		lines := []string{
			"links agent quickstart",
			"",
			"1) Discover context",
			"   `lit hooks install --json`",
			"   `lit workspace --json`",
			"",
			"2) Find work",
			"   `lit ls --format lines --json`",
			"   `lit ls --query \"status:open type:task\" --sort priority:asc,updated_at:desc --json`",
			"",
			"3) Create and relate issues/epics",
			"   `lit new --title \"...\" --type task|bug|feature|chore|epic --json`",
			"   `lit parent set <child-id> <parent-id> --json`",
			"   `lit dep add <src-id> <dst-id> --type blocks|related-to|parent-child --json`",
			"",
			"4) Safe mutations",
			"   Read `workspace_revision` and pass `--expected-revision N` on writes.",
			"",
			"5) Dolt remote sync",
			"   Configure remotes with git, then run sync commands.",
			"   `git remote -v`",
			"   `lit sync remote ls --json`",
			"   `lit sync fetch --remote origin --json`",
			"   `lit sync pull --remote origin --branch main --json`",
			"   `lit sync push --remote origin --branch main --json`",
			"   If sync warning appears, agent should auto-retry silently before escalating.",
			"",
			"6) Integrity and recovery",
			"   `lit doctor --json`",
			"   `lit fsck --repair --json`",
			"   `lit backup create --json`",
			"   `lit backup restore --latest --json`",
			"   `lit recover --latest-backup --json`",
			"",
			fmt.Sprintf("Exit codes: ok=%d usage=%d validation=%d not_found=%d conflict=%d stale_revision=%d corruption=%d", ExitOK, ExitUsage, ExitValidation, ExitNotFound, ExitConflict, ExitStaleRevision, ExitCorruption),
		}
		if summary, ok := instructions["summary"].(string); ok && strings.TrimSpace(summary) != "" {
			lines[0] = summary
		}
		_, err := fmt.Fprintln(w, strings.Join(lines, "\n"))
		return err
	})
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printValue(w io.Writer, v any, jsonOut bool, textFn func(io.Writer, any) error) error {
	if jsonOut {
		return writeJSON(w, v)
	}
	return textFn(w, v)
}

func printIssueSummary(w io.Writer, v any) error {
	issue := v.(model.Issue)
	_, err := fmt.Fprintf(w, "%s [%s/%s/P%d] %s%s\n", issue.ID, formatIssueState(issue), issue.IssueType, issue.Priority, issue.Title, formatLabels(issue.Labels))
	return err
}

func printIssueTable(w io.Writer, issues []model.Issue, columns []string) error {
	resolved := resolveColumns(columns)
	tw := tabwriter.NewWriter(w, 2, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.ToUpper(strings.Join(resolved, "\t"))); err != nil {
		return err
	}
	for _, issue := range issues {
		if _, err := fmt.Fprintln(tw, formatIssueColumns(issue, resolved, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func printIssueLines(w io.Writer, issues []model.Issue, columns []string) error {
	resolved := resolveColumns(columns)
	for _, issue := range issues {
		if _, err := fmt.Fprintln(w, formatIssueColumns(issue, resolved, " | ")); err != nil {
			return err
		}
	}
	return nil
}

func printIssueDetail(w io.Writer, detail model.IssueDetail) error {
	issue := detail.Issue
	if _, err := fmt.Fprintf(w, "%s\n%s\n\nstatus: %s\ntype: %s\npriority: %d\nassignee: %s\nlabels: %s\narchived: %s\ndeleted: %s\n", issue.ID, issue.Title, issue.Status, issue.IssueType, issue.Priority, emptyDash(issue.Assignee), emptyDash(strings.Join(issue.Labels, ", ")), formatOptionalTime(issue.ArchivedAt), formatOptionalTime(issue.DeletedAt)); err != nil {
		return err
	}
	if issue.Description != "" {
		if _, err := fmt.Fprintf(w, "\ndescription:\n%s\n", issue.Description); err != nil {
			return err
		}
	}
	if detail.Parent != nil {
		if _, err := fmt.Fprintf(w, "\nparent:\n- %s %s\n", detail.Parent.ID, detail.Parent.Title); err != nil {
			return err
		}
	}
	if err := printIssueGroup(w, "children", detail.Children); err != nil {
		return err
	}
	if err := printIssueGroup(w, "depends_on", detail.DependsOn); err != nil {
		return err
	}
	if err := printIssueGroup(w, "blocked_by", detail.BlockedBy); err != nil {
		return err
	}
	if err := printIssueGroup(w, "related", detail.Related); err != nil {
		return err
	}
	if len(detail.Comments) > 0 {
		if _, err := fmt.Fprintln(w, "\ncomments:"); err != nil {
			return err
		}
		for _, c := range detail.Comments {
			if _, err := fmt.Fprintf(w, "- [%s] %s\n", c.CreatedBy, strings.ReplaceAll(c.Body, "\n", "\\n")); err != nil {
				return err
			}
		}
	}
	if len(detail.History) > 0 {
		if _, err := fmt.Fprintln(w, "\nhistory:"); err != nil {
			return err
		}
		for _, event := range detail.History {
			if _, err := fmt.Fprintf(w, "- [%s] %s %s (%s -> %s)\n", event.CreatedBy, event.Action, strings.ReplaceAll(event.Reason, "\n", "\\n"), emptyDash(event.FromStatus), emptyDash(event.ToStatus)); err != nil {
				return err
			}
		}
	}
	return nil
}

func printIssueGroup(w io.Writer, label string, issues []model.Issue) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s:\n", label); err != nil {
		return err
	}
	for _, issue := range issues {
		if _, err := fmt.Fprintf(w, "- %s %s\n", issue.ID, issue.Title); err != nil {
			return err
		}
	}
	return nil
}

func formatIssueColumns(issue model.Issue, columns []string, delimiter string) string {
	values := make([]string, 0, len(columns))
	for _, column := range columns {
		switch column {
		case "id":
			values = append(values, issue.ID)
		case "state":
			values = append(values, formatIssueState(issue))
		case "type":
			values = append(values, issue.IssueType)
		case "priority":
			values = append(values, strconv.Itoa(issue.Priority))
		case "title":
			values = append(values, issue.Title)
		case "assignee":
			values = append(values, emptyDash(issue.Assignee))
		case "labels":
			values = append(values, emptyDash(strings.Join(issue.Labels, ",")))
		case "updated_at":
			values = append(values, issue.UpdatedAt.Format(time.RFC3339))
		case "created_at":
			values = append(values, issue.CreatedAt.Format(time.RFC3339))
		}
	}
	return strings.Join(values, delimiter)
}

func resolveColumns(columns []string) []string {
	if len(columns) == 0 {
		// [LAW:dataflow-not-control-flow] Default listing still flows through the same projection path.
		return []string{"id", "state", "title"}
	}
	valid := map[string]struct{}{
		"id": {}, "state": {}, "type": {}, "priority": {}, "title": {}, "assignee": {}, "labels": {}, "updated_at": {}, "created_at": {},
	}
	out := make([]string, 0, len(columns))
	for _, column := range columns {
		normalized := strings.ToLower(strings.TrimSpace(column))
		if normalized == "" {
			continue
		}
		if _, ok := valid[normalized]; ok {
			out = append(out, normalized)
		}
	}
	if len(out) == 0 {
		return []string{"id", "state", "title"}
	}
	return out
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func printLabels(w io.Writer, v any) error {
	labels := v.([]string)
	_, err := fmt.Fprintln(w, strings.Join(labels, ","))
	return err
}

func formatLabels(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	return " [" + strings.Join(labels, ",") + "]"
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.Format(time.RFC3339)
}

func transitionCommandName(action string) string {
	switch action {
	case "reopen":
		return "open"
	default:
		return action
	}
}

func formatIssueState(issue model.Issue) string {
	parts := []string{issue.Status}
	if issue.ArchivedAt != nil {
		parts = append(parts, "archived")
	}
	if issue.DeletedAt != nil {
		parts = append(parts, "deleted")
	}
	return strings.Join(parts, "+")
}

func splitCSV(input string) []string {
	if strings.TrimSpace(input) == "" {
		return nil
	}
	parts := strings.Split(input, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func parseColumns(input string) []string {
	return splitCSV(strings.ToLower(input))
}

func parseSortSpecs(input string) ([]store.SortSpec, error) {
	parts := splitCSV(input)
	if len(parts) == 0 {
		return nil, nil
	}
	out := make([]store.SortSpec, 0, len(parts))
	for _, part := range parts {
		spec := strings.TrimSpace(part)
		field := spec
		desc := false
		if strings.Contains(spec, ":") {
			chunks := strings.SplitN(spec, ":", 2)
			field = strings.TrimSpace(chunks[0])
			direction := strings.ToLower(strings.TrimSpace(chunks[1]))
			switch direction {
			case "asc":
				desc = false
			case "desc":
				desc = true
			default:
				return nil, fmt.Errorf("unsupported sort direction %q", direction)
			}
		}
		out = append(out, store.SortSpec{Field: field, Desc: desc})
	}
	return out, nil
}

func optionalExpectedRevision(fs *flag.FlagSet, value *int64) *int64 {
	if fs.Lookup("expected-revision") == nil {
		return nil
	}
	visited := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "expected-revision" {
			visited = true
		}
	})
	if !visited {
		return nil
	}
	out := *value
	return &out
}

func syncBasePath(ap *app.App) string {
	return filepath.Join(ap.Workspace.StorageDir, "last-sync-base.json")
}

func restoreFromExportPath(ctx context.Context, ap *app.App, path string, force bool) error {
	restorePath := filepath.Clean(path)
	targetExport, _, err := syncfile.Read(restorePath)
	if err != nil {
		return err
	}
	localExport, err := ap.Store.Export(ctx)
	if err != nil {
		return err
	}
	state, err := ap.Store.GetSyncState(ctx)
	if err != nil {
		return err
	}
	currentRevision, err := ap.Store.GetWorkspaceRevision(ctx)
	if err != nil {
		return err
	}
	if state.WorkspaceRevision != 0 && currentRevision != state.WorkspaceRevision && !force {
		return MergeConflictError{Message: fmt.Sprintf("restore conflict: local workspace revision %d has unsynced changes since last sync revision %d", currentRevision, state.WorkspaceRevision)}
	}
	if _, err := backup.Create(ap.Workspace.StorageDir, localExport); err != nil {
		return err
	}
	if err := backup.Prune(ap.Workspace.StorageDir, 20); err != nil {
		return err
	}
	if err := ap.Store.ReplaceFromExport(ctx, targetExport); err != nil {
		return err
	}
	if _, err := syncfile.WriteAtomic(syncBasePath(ap), targetExport); err != nil {
		return err
	}
	hash, err := syncfile.HashFile(restorePath)
	if err != nil {
		return err
	}
	return ap.Store.RecordSyncState(ctx, store.SyncState{
		Path:              restorePath,
		ContentHash:       hash,
		WorkspaceRevision: targetExport.WorkspaceRevision,
	})
}

type MergeConflictError struct {
	Message   string
	Conflicts []merge.IssueConflict
}

func (e MergeConflictError) Error() string {
	return e.Message
}

type CorruptionError struct {
	Message string
}

func (e CorruptionError) Error() string { return e.Message }

func printUsage(w io.Writer) {
	fmt.Fprint(w, `links / lit

Usage:
  lit new --title <title> [--description <text>] [--type task|feature|bug|chore|epic] [--priority 0-4] [--assignee <user>] [--labels a,b] [--expected-revision N] [--json]
  lit ls [--status open|closed] [--type <type>] [--assignee <user>] [--priority-min N] [--priority-max N] [--search <text>] [--ids a,b] [--labels a,b] [--has-comments] [--include-archived] [--include-deleted] [--updated-after RFC3339] [--updated-before RFC3339] [--query <expr>] [--sort field:asc|desc,...] [--columns id,state,title,...] [--format lines|table] [--limit N] [--json]
  lit show <id> [--json]
  lit edit <id> [--title ...] [--description ...] [--type ...] [--priority ...] [--assignee ...|--clear-assignee] [--labels a,b|--clear-labels] [--expected-revision N] [--json]
  lit close <id> --reason <text> [--by <user>] [--expected-revision N] [--json]
  lit open <id> --reason <text> [--by <user>] [--expected-revision N] [--json]
  lit archive <id> --reason <text> [--by <user>] [--expected-revision N] [--json]
  lit delete <id> --reason <text> [--by <user>] [--expected-revision N] [--json]
  lit unarchive <id> --reason <text> [--by <user>] [--expected-revision N] [--json]
  lit restore <id> --reason <text> [--by <user>] [--expected-revision N] [--json]
  lit comment add <id> --body <text> [--by <user>] [--expected-revision N] [--json]
  lit label add <issue-id> <label> [--by <user>] [--expected-revision N] [--json]
  lit label rm <issue-id> <label> [--expected-revision N] [--json]
  lit parent set <child-id> <parent-id> [--by <user>] [--expected-revision N] [--json]
  lit parent clear <child-id> [--expected-revision N] [--json]
  lit children <parent-id> [--json]
  lit dep add <src-id> <dst-id> [--type blocks|parent-child|related-to] [--by <user>] [--expected-revision N] [--json]
  lit dep rm <src-id> <dst-id> [--type blocks|parent-child|related-to] [--expected-revision N] [--json]
  lit dep ls <issue-id> [--type blocks|parent-child|related-to] [--json]
  lit export [--json]
  lit sync status [--json]
  lit sync remote ls [--json]
  lit sync fetch [--remote <name>] [--prune] [--json]
  lit sync pull [--remote <name>] [--branch <name>] [--json]
  lit sync push [--remote <name>] [--branch <name>] [--set-upstream] [--force] [--json]
  lit doctor [--json]
  lit fsck [--repair] [--json]
  lit backup create [--keep N] [--json]
  lit backup list [--json]
  lit backup restore (--path <snapshot.json> | --latest) [--force] [--json]
  lit recover (--from-sync <path> | --from-backup <path> | --latest-backup) [--force] [--json]
  lit bulk label <add|rm> --ids a,b --label <name> [--by <user>] [--expected-revision N] [--json]
  lit bulk <close|archive> --ids a,b --reason <text> [--by <user>] [--expected-revision N] [--json]
  lit bulk import --path <export.json> [--force] [--json]
  lit hooks install [--json]
  lit quickstart [--json]
  lit completion <bash|zsh|fish>
  lit beads import --db <path> [--json]
  lit beads export --db <path> [--json]
  lit workspace [--json]
`)
}

func splitArgs(args []string, positionalCount int) ([]string, []string) {
	positionals := make([]string, 0, positionalCount)
	flags := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
				flags = append(flags, args[index+1])
				index++
			}
			continue
		}
		if len(positionals) < positionalCount {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
	}
	return positionals, flags
}
