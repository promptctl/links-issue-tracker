package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bmf/links-issue-tracker/internal/app"
	"github.com/bmf/links-issue-tracker/internal/store"
)

func resolveDoctorAccessMode(args []string) appAccessMode {
	cmd := &cobra.Command{Use: "doctor"}
	fix := cmd.Flags().String("fix", "", "")
	cmd.Flags().Lookup("fix").NoOptDefVal = "all"
	cmd.Flags().Bool("json", false, "")
	resetPreMigration := cmd.Flags().Bool("reset-to-pre-migration", false, "")
	if err := cmd.ParseFlags(args); err != nil {
		return appAccessWrite
	}
	if *fix != "" || *resetPreMigration {
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
	resetPreMigration := fs.Bool("reset-to-pre-migration", false,
		"Revert to the most recent pre-migrate safety branch and quarantine the migrations applied since")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if *resetPreMigration {
		// Reset is a destructive recovery surface — run it before any
		// other doctor work so a broken-schema workspace can't fail in
		// the smoke probes below.
		return runDoctorResetToPreMigration(ctx, stdout, ap, *jsonOut)
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
		r := v.(store.HealthReport)
		_, err := fmt.Fprintf(w, "integrity_check=%s smoke_test=%s foreign_key_issues=%d invalid_related_rows=%d orphan_history_rows=%d rank_inversions=%d\n", r.IntegrityCheck, r.SmokeTest, r.ForeignKeyIssues, r.InvalidRelatedRows, r.OrphanHistoryRows, r.RankInversions)
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

// runDoctorResetToPreMigration drives the recovery surface for agents whose
// schema is broken at HEAD. The Store implements the actual revert +
// quarantine; the CLI just renders the result so the agent sees what
// happened. [LAW:single-enforcer] The recovery flow lives in the store
// (ResetToPreMigration); the CLI is a thin renderer.
func runDoctorResetToPreMigration(ctx context.Context, stdout io.Writer, ap *app.App, jsonOut bool) error {
	result, err := ap.Store.ResetToPreMigration(ctx)
	if err != nil {
		return err
	}
	return printValue(stdout, result, jsonOut, func(w io.Writer, v any) error {
		r := v.(store.ResetToPreMigrationResult)
		versions := make([]string, 0, len(r.QuarantinedVersions))
		for _, ver := range r.QuarantinedVersions {
			versions = append(versions, fmt.Sprintf("%d", ver))
		}
		quarantined := "(none)"
		if len(versions) > 0 {
			quarantined = strings.Join(versions, ",")
		}
		_, err := fmt.Fprintf(w, "Reset to %s (created %s); quarantined versions: %s\n",
			r.Checkpoint, r.CheckpointTimestamp, quarantined)
		return err
	})
}
