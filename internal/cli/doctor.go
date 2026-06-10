package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// printWorkspaceIdentity writes the resolved store identity so a human can
// confirm at a glance which store lit opened — the direct answer to "why am I
// not seeing my issues". It is part of the text report only: --json emits a
// single JSON document and nothing else, so machine consumers are never handed
// a non-JSON line. [LAW:one-source-of-truth] Renders the already-resolved
// workspace.Info; it never re-resolves storage location.
func printWorkspaceIdentity(w io.Writer, ws workspace.Info) error {
	// Path fields use %q so values containing spaces (e.g. a checkout under
	// "My Projects") stay unambiguous and copy-pasteable in the key=value line.
	prefixSource := "configured"
	if ws.IssuePrefix.Derived() {
		prefixSource = "derived"
	}
	_, err := fmt.Fprintf(w, "workspace: storage_dir=%q workspace_id=%s issue_prefix=%s issue_prefix_source=%s git_common_dir=%q\n",
		ws.StorageDir, ws.WorkspaceID, ws.IssuePrefix.Value(), prefixSource, ws.GitCommonDir)
	return err
}

func resolveDoctorAccessMode(args []string) appAccessMode {
	cmd := &cobra.Command{Use: "doctor"}
	fix := cmd.Flags().String("fix", "", "")
	cmd.Flags().Lookup("fix").NoOptDefVal = "all"
	cmd.Flags().Bool("json", false, "")
	if err := cmd.ParseFlags(args); err != nil {
		return appAccessWrite
	}
	if *fix != "" {
		return appAccessWrite
	}
	return appAccessRead
}

func allDoctorFixNames() []string {
	names := make([]string, 0, len(doctorFixes))
	for k := range doctorFixes {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// doctorFixes is the registry of available doctor fixes.
// [LAW:one-source-of-truth] This map is the single authority for valid fix names.
var doctorFixes = map[string]func(context.Context, io.Writer, *app.App) error{
	"integrity": func(ctx context.Context, w io.Writer, ap *app.App) error {
		report, err := ap.Store.Fsck(ctx, true)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "Integrity repair: foreign_key_issues=%d invalid_related_rows=%d orphan_history_rows=%d\n",
			report.ForeignKeyIssues, report.InvalidRelatedRows, report.OrphanHistoryRows)
		return err
	},
	"rank": func(ctx context.Context, w io.Writer, ap *app.App) error {
		fixed, err := ap.Store.FixRankInversions(ctx)
		if err != nil {
			return err
		}
		if fixed > 0 {
			_, err = fmt.Fprintf(w, "Re-ranked %d dependency issue(s) to repair rank order.\n", fixed)
		}
		return err
	},
}

func runDoctor(ctx context.Context, stdout io.Writer, ap *app.App, args []string) error {
	fs := newCobraFlagSet("doctor")
	fix := fs.String("fix", "", "Apply fixes: --fix (all) or --fix rank,thingA")
	fs.cmd.Flags().Lookup("fix").NoOptDefVal = "all"
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if *fix != "" {
		fixNames := allDoctorFixNames()
		if *fix != "all" {
			fixNames = splitCSV(*fix)
		}
		// [LAW:dataflow-not-control-flow] Fix progress always writes to stderr
		// so stdout remains clean for the JSON report when --json is set.
		for _, name := range fixNames {
			fn, ok := doctorFixes[name]
			if !ok {
				return fmt.Errorf("unknown fix %q; available: %s", name, strings.Join(allDoctorFixNames(), ", "))
			}
			if err := fn(ctx, os.Stderr, ap); err != nil {
				return err
			}
		}
	}
	report, err := ap.Store.Doctor(ctx)
	if err != nil {
		return err
	}
	if err := printValue(stdout, report, *jsonOut, func(w io.Writer, v any) error {
		// The identity header is part of the text rendering only; routing it
		// through the text closure makes JSON mode structurally unable to emit it.
		if err := printWorkspaceIdentity(w, ap.Workspace); err != nil {
			return err
		}
		r := v.(store.HealthReport)
		dependencyCycle := "none"
		if len(r.DependencyCycle) > 0 {
			dependencyCycle = strings.Join(r.DependencyCycle, "->")
		}
		_, err := fmt.Fprintf(w, "integrity_check=%s foreign_key_issues=%d invalid_related_rows=%d orphan_history_rows=%d rank_inversions=%d update_dryrun_failures=%d dependency_cycle=%s\n", r.IntegrityCheck, r.ForeignKeyIssues, r.InvalidRelatedRows, r.OrphanHistoryRows, r.RankInversions, r.UpdateDryRunFailures, dependencyCycle)
		return err
	}); err != nil {
		return err
	}
	// [LAW:single-enforcer] Corruption classification is output-format agnostic and always enforced here.
	if len(report.Errors) > 0 {
		return CorruptionError{Message: strings.Join(report.Errors, "; ")}
	}
	return nil
}
