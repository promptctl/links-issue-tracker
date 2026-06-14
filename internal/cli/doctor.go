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

// doctorSyncKind is the outer discriminant of doctor's freshness view: the two
// outcomes only the CLI can determine (no git remote is configured, or
// freshness could not be resolved) plus the resolved case, where the store's
// SyncFreshness.State carries the inner ahead/behind classification. Keeping
// these orthogonal to the store's states avoids a second enum that could drift
// from store.SyncFreshnessState. [LAW:one-source-of-truth]
type doctorSyncKind int

const (
	doctorSyncNoRemote doctorSyncKind = iota
	doctorSyncUnresolved
	doctorSyncResolved
)

// doctorSyncReport is the rendered-once value for the sync freshness line. Kind
// is the single discriminant the renderer switches on; freshness is meaningful
// only when Kind is doctorSyncResolved and Detail only when doctorSyncUnresolved.
type doctorSyncReport struct {
	Kind      doctorSyncKind
	Freshness store.SyncFreshness
	Detail    string
}

// resolveDoctorSyncFreshness computes the sync freshness view. It performs the
// git/store reads (effects) so the print closure stays pure
// [LAW:effects-at-boundaries], and it never returns an error: a failure becomes
// a loud doctorSyncUnresolved line carrying the reason [LAW:no-silent-failure]
// rather than aborting doctor, because sync freshness is a best-effort
// diagnostic distinct from the integrity health check it sits beside.
func resolveDoctorSyncFreshness(ctx context.Context, ws workspace.Info, st *store.Store) doctorSyncReport {
	gitRemotes, err := workspace.GitRemotes(ws.RootDir)
	if err != nil {
		return doctorSyncReport{Kind: doctorSyncUnresolved, Detail: fmt.Sprintf("read git remotes: %v", err)}
	}
	// [LAW:one-source-of-truth] Resolve the same remote+branch `lit sync` uses,
	// so doctor's freshness reflects exactly what `lit sync push/pull` act on.
	remoteName, err := resolveSyncRemote("", workspace.UpstreamRemote(ws.RootDir), gitRemotes)
	if err != nil {
		return doctorSyncReport{Kind: doctorSyncUnresolved, Detail: err.Error()}
	}
	if remoteName == "" {
		return doctorSyncReport{Kind: doctorSyncNoRemote}
	}
	branch, err := resolveSyncBranch(ws.RootDir, remoteName)
	if err != nil {
		return doctorSyncReport{Kind: doctorSyncUnresolved, Detail: err.Error()}
	}
	freshness, err := st.SyncFreshness(ctx, remoteName, branch)
	if err != nil {
		return doctorSyncReport{Kind: doctorSyncUnresolved, Detail: err.Error()}
	}
	return doctorSyncReport{Kind: doctorSyncResolved, Freshness: freshness}
}

// printSyncFreshness renders the freshness line. Every resolved state names the
// stale direction and the exact command to fix it; the behind direction is
// always qualified "as of last fetch" because it is read from the local
// remote-tracking ref, which doctor does not refresh over the network.
func printSyncFreshness(w io.Writer, report doctorSyncReport) error {
	switch report.Kind {
	case doctorSyncNoRemote:
		_, err := fmt.Fprintln(w, "sync: no git remote configured — ticket history stays on this machine; add a remote and run 'lit sync push' to share it")
		return err
	case doctorSyncUnresolved:
		_, err := fmt.Fprintf(w, "sync: freshness unavailable — %s\n", report.Detail)
		return err
	}
	f := report.Freshness
	ref := f.Remote + "/" + f.Branch
	switch f.State() {
	case store.SyncNeverSynced:
		_, err := fmt.Fprintf(w, "sync: never synced with %s — run 'lit sync push' to publish local tickets ('lit sync pull' to receive remote ones)\n", ref)
		return err
	case store.SyncUpToDate:
		_, err := fmt.Fprintf(w, "sync: up to date with %s (as of last fetch)\n", ref)
		return err
	case store.SyncAhead:
		_, err := fmt.Fprintf(w, "sync: ahead of %s by %d local change(s) not pushed — run 'lit sync push' [ahead=%d behind=0]\n", ref, f.Ahead, f.Ahead)
		return err
	case store.SyncBehind:
		_, err := fmt.Fprintf(w, "sync: behind %s by %d change(s) not pulled, as of last fetch — run 'lit sync pull' [ahead=0 behind=%d]\n", ref, f.Behind, f.Behind)
		return err
	case store.SyncDiverged:
		_, err := fmt.Fprintf(w, "sync: diverged from %s — %d local change(s) not pushed, %d remote change(s) not pulled as of last fetch; run 'lit sync pull' to reconcile [ahead=%d behind=%d]\n", ref, f.Ahead, f.Behind, f.Ahead, f.Behind)
		return err
	}
	// [LAW:no-silent-failure] State() is exhaustive over the store's freshness
	// states; a value here means a new state was added without a render arm.
	return fmt.Errorf("unhandled sync freshness state %q", f.State())
}

func resolveDoctorAccessMode(args []string) app.AccessMode {
	cmd := &cobra.Command{Use: "doctor"}
	fix := cmd.Flags().String("fix", "", "")
	cmd.Flags().Lookup("fix").NoOptDefVal = "all"
	cmd.Flags().Bool("json", false, "")
	if err := cmd.ParseFlags(args); err != nil {
		return app.AccessWrite
	}
	if *fix != "" {
		return app.AccessWrite
	}
	return app.AccessRead
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
	fs.JSONFlag()
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
	// Resolve freshness here, outside the print closure, so the closure stays a
	// pure renderer. [LAW:effects-at-boundaries] Like the identity header, the
	// freshness line is part of the text rendering only, so --json still emits a
	// single HealthReport document and nothing else.
	syncReport := resolveDoctorSyncFreshness(ctx, ap.Workspace, ap.Store)
	if err := printValue(stdout, report, func(w io.Writer, v any) error {
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
		if _, err := fmt.Fprintf(w, "integrity_check=%s foreign_key_issues=%d invalid_related_rows=%d orphan_history_rows=%d rank_inversions=%d update_dryrun_failures=%d dependency_cycle=%s\n", r.IntegrityCheck, r.ForeignKeyIssues, r.InvalidRelatedRows, r.OrphanHistoryRows, r.RankInversions, r.UpdateDryRunFailures, dependencyCycle); err != nil {
			return err
		}
		return printSyncFreshness(w, syncReport)
	}); err != nil {
		return err
	}
	// [LAW:single-enforcer] Corruption classification is output-format agnostic and always enforced here.
	if len(report.Errors) > 0 {
		return CorruptionError{Message: strings.Join(report.Errors, "; ")}
	}
	return nil
}
