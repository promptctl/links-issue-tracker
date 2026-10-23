package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// printWorkspaceIdentity writes the resolved store identity so a human can
// confirm at a glance which store lit opened — the direct answer to "why am I
// not seeing my issues". [LAW:one-source-of-truth] Renders the already-resolved
// workspace.Info; it never re-resolves storage location.
//
// issue_prefix is the text config.json carries, legal or not, and id_prefixes is
// what the store's ids actually use, side by side, because the second is the
// evidence the first is judged against.
func printWorkspaceIdentity(w io.Writer, ws workspace.Info, census idPrefixCensus) error {
	// Path fields use %q so values containing spaces (e.g. a checkout under
	// "My Projects") stay unambiguous and copy-pasteable in the key=value line.
	// issue_prefix is quoted for the same reason: a refused prefix is whatever
	// text config.json carries, spaces and `=` included.
	prefixSource := "configured"
	if ws.IssuePrefix.Derived() {
		prefixSource = "derived"
	}
	_, err := fmt.Fprintf(w, "workspace: storage_dir=%q workspace_id=%s issue_prefix=%q issue_prefix_source=%s id_prefixes=%s git_common_dir=%q\n",
		ws.StorageDir, ws.WorkspaceID, ws.IssuePrefix.Stored(), prefixSource, census, ws.GitCommonDir)
	return err
}

// printPrefixMismatch renders the prefix line when — and only when — config.json
// names a prefix none of the store's ids carry. Two arrivals produce it and one
// sentence serves both: a stored value the rules refuse (no id was ever minted
// under it), and a legal value sitting over a backlog minted under another, as
// adopting a remote backlog into a differently-prefixed workspace leaves it.
// Straight after a deliberate `lit prefix set` it is expected, so it is a report
// and never a failure; the command it names previews before anything is written.
func printPrefixMismatch(w io.Writer, prefix workspace.PrefixState, census idPrefixCensus) error {
	if !census.mismatch(prefix.Stored()) {
		return nil
	}
	_, err := fmt.Fprintf(w, "prefix: issue_prefix %q matches none of this store's issue ids (%s) — run 'lit prefix set %s' to preview the change\n",
		prefix.Stored(), census, census.adoptable())
	return err
}

// doctorSyncKind is the outer discriminant of doctor's freshness view: the two
// outcomes only the CLI can determine (no git remote is configured, or
// freshness could not be resolved) plus the resolved case, where the store's
// SyncFreshness.State carries the inner ahead/behind classification. Keeping
// these orthogonal to the store's states avoids a second enum that could drift
// from storage.SyncFreshnessState. [LAW:one-source-of-truth]
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
	Freshness storage.SyncFreshness
	Detail    string
	// Age is the divergence's age, computed at the resolve boundary from the
	// freshness's oldest-divergent-commit timestamp. Zero unless diverged; it is
	// what lets doctor tell a fresh divergence (in-progress reconcile) from one
	// that has festered into an incident.
	Age time.Duration
}

// divergenceFailure returns the sync-failure contract for a diverged workspace,
// or ok=false for any non-diverged freshness. The age rides on the report
// (computed at the resolve boundary), so this is a mostly-pure derivation — the
// same value drives both the rendered escalation and the exit decision — except
// for the resolveBuildStatusNote call, which reads this binary's build identity
// at call time rather than from the report. [LAW:dataflow-not-control-flow]
func (r doctorSyncReport) divergenceFailure() (SyncFailure, bool) {
	if r.Kind != doctorSyncResolved || r.Freshness.State() != storage.SyncDiverged {
		return SyncFailure{}, false
	}
	return SyncFailure{
		Class:     syncFailureDivergedUnresolved,
		Remote:    r.Freshness.Remote,
		Branch:    r.Freshness.Branch,
		Ahead:     r.Freshness.Ahead,
		Behind:    r.Freshness.Behind,
		Age:       r.Age,
		BuildNote: resolveBuildStatusNote(time.Now()),
	}, true
}

// doctorDivergenceExit is the one place doctor turns a persistent divergence into
// a nonzero exit: a divergence past the persistence threshold is an incident, not
// a diagnostic note, so a health check must not pass while the workspace is
// silently stuck. Kept mostly-pure over the already-resolved report — divergenceFailure's
// build-identity read is the one exception — so the exit contract is
// unit-testable without a store aged for real. [LAW:no-silent-failure]
func doctorDivergenceExit(report doctorSyncReport) error {
	if failure, diverged := report.divergenceFailure(); diverged && failure.persistent() {
		return SyncFailureError{Failure: failure}
	}
	return nil
}

// resolveDoctorSyncFreshness computes the sync freshness view. It performs the
// git/store reads (effects) so the print closure stays pure
// [LAW:effects-at-boundaries], and it never returns an error: a failure becomes
// a loud doctorSyncUnresolved line carrying the reason [LAW:no-silent-failure]
// rather than aborting doctor, because sync freshness is a best-effort
// diagnostic distinct from the integrity health check it sits beside.
func resolveDoctorSyncFreshness(ctx context.Context, env environ.Env, ws workspace.Info, st storage.Store) doctorSyncReport {
	// An engine that does not sync has no freshness to report, which is the same
	// kind of answer as a git remote that cannot be read: unresolved, with the
	// reason named. It reaches the reader through the one arm this function
	// already has for that, rather than through a silent omission.
	// [LAW:no-silent-failure]
	syncer, err := storage.Sync.Of(st)
	if err != nil {
		return doctorSyncReport{Kind: doctorSyncUnresolved, Detail: err.Error()}
	}
	gitRemotes, err := workspace.GitRemotes(ctx, env, ws.RootDir)
	if err != nil {
		return doctorSyncReport{Kind: doctorSyncUnresolved, Detail: fmt.Sprintf("read git remotes: %v", err)}
	}
	// [LAW:one-source-of-truth] Resolve the same remote `lit sync` uses, so
	// doctor's freshness reflects exactly what `lit sync push/pull` act on; the
	// branch is knownSyncBranch's, not resolveSyncBranch's, because this report
	// is read by doctor and by every read command's banner, and neither waits
	// on the network.
	remoteName, branch, ok, err := freshnessSyncTarget(ctx, env, ws, gitRemotes)
	if err != nil {
		return doctorSyncReport{Kind: doctorSyncUnresolved, Detail: err.Error()}
	}
	if !ok {
		return doctorSyncReport{Kind: doctorSyncNoRemote}
	}
	freshness, err := syncer.SyncFreshness(ctx, remoteName, branch)
	if err != nil {
		return doctorSyncReport{Kind: doctorSyncUnresolved, Detail: err.Error()}
	}
	// The clock meets the stored timestamp here, at the effect boundary, so the
	// printed line and the exit decision below both read one already-computed age.
	// [LAW:effects-at-boundaries]
	return doctorSyncReport{
		Kind:      doctorSyncResolved,
		Freshness: freshness,
		Age:       ageFromOldestDivergedUnix(freshness.OldestDivergedUnix, time.Now()),
	}
}

// printPushOutcomeHealth renders the last-push-failure line when — and only
// when — the last push attempt did not land. It is what makes the detached
// mirror's failure sink discoverable from the one surface users are told to
// check: the banner elsewhere compresses the failure to one line, while doctor
// names the durable record (mirror.log, with its last-write age) that holds
// the full story. A healthy or never-pushed workspace prints nothing — the
// freshness line above already covers those states, and a second line about
// them would be a drifting copy. [LAW:one-source-of-truth]
//
// The line is a past observation and says so: its age, the provenance clause
// when the binary has changed since, and `lit sync push`, the command that
// produces a current verdict. Doctor is read-only, so it cannot re-test a push.
func printPushOutcomeHealth(w io.Writer, ws workspace.Info, now time.Time, running string) error {
	rec, age, known := lastPushOutcome(ws, now)
	if !known || !rec.failed() {
		return nil
	}
	logNote := ""
	logPath := filepath.Join(ws.StorageDir, mirrorLogName)
	if info, err := os.Stat(logPath); err == nil {
		logNote = fmt.Sprintf(" — mirror log: %s (last written %s ago)", logPath, humanizeCoarseDuration(now.Sub(info.ModTime())))
	}
	_, err := fmt.Fprintf(w, "sync: last push attempt FAILED %s ago%s: %s%s — run 'lit sync push' to re-test\n",
		humanizeCoarseDuration(age), pushOutcomeProvenance(rec, running), oneLineReason(rec.Reason), logNote)
	return err
}

// printSyncFreshness renders the freshness line. Every resolved state names the
// stale direction and the exact command to fix it, qualified "as of last fetch"
// because both ahead and behind are read from the local remote-tracking ref,
// which doctor does not refresh over the network.
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
	case storage.SyncNeverSynced:
		_, err := fmt.Fprintf(w, "sync: never synced with %s — run 'lit sync push' to publish local tickets ('lit sync pull' to receive remote ones)\n", ref)
		return err
	case storage.SyncUpToDate:
		_, err := fmt.Fprintf(w, "sync: up to date with %s (as of last fetch)\n", ref)
		return err
	case storage.SyncAhead:
		_, err := fmt.Fprintf(w, "sync: ahead of %s by %d local change(s) not pushed, as of last fetch — run 'lit sync push' [ahead=%d behind=0]\n", ref, f.Ahead, f.Ahead)
		return err
	case storage.SyncBehind:
		_, err := fmt.Fprintf(w, "sync: behind %s by %d change(s) not pulled, as of last fetch — run 'lit sync pull' [ahead=0 behind=%d]\n", ref, f.Behind, f.Behind)
		return err
	case storage.SyncDiverged:
		_, err := fmt.Fprintf(w, "sync: diverged from %s as of last fetch — %d local change(s) not pushed, %d remote change(s) not pulled; run 'lit sync pull' to reconcile [ahead=%d behind=%d]\n", ref, f.Ahead, f.Behind, f.Ahead, f.Behind)
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
// doctorFix repairs one class of fault.
//
// climbsHierarchy marks a repair whose work begins with the live-issue
// classification, a hydrating read — and a hydrating read refuses a hierarchy
// that holds a loop (storage.ParentCycleError). Carrying it as data on the
// repair, rather than as a branch where the repairs are dispatched, is what
// keeps the skip honest: a repair that can run is not withheld, and a new
// repair declares its own reach instead of some caller guessing at it.
// [LAW:types-are-the-program] [LAW:dataflow-not-control-flow]
type doctorFix struct {
	// name is filled in from the map key at parse time, so the two cannot drift.
	name            string
	climbsHierarchy bool
	run             func(context.Context, io.Writer, storage.Repairer) error
}

var doctorFixes = map[string]doctorFix{
	// FixIntegrity is three statements over issue_events and related-to rows;
	// it never reads the hierarchy, so a loop does not stop it.
	"integrity": {run: func(ctx context.Context, w io.Writer, repairer storage.Repairer) error {
		report, err := repairer.FixIntegrity(ctx)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "Integrity repair: foreign_key_issues=%d invalid_related_rows=%d orphan_history_rows=%d\n",
			report.ForeignKeyIssues, report.InvalidRelatedRows, report.OrphanHistoryRows)
		return err
	}},
	// FixRankInversions starts from the live-issue classification, which climbs.
	"rank": {climbsHierarchy: true, run: func(ctx context.Context, w io.Writer, repairer storage.Repairer) error {
		fixed, err := repairer.FixRankInversions(ctx)
		if err != nil {
			return err
		}
		telemetry.Set(ctx, "doctor.reranked", fixed)
		if fixed > 0 {
			// The count is issues MOVED, not dependencies hoisted: the repair
			// writes whichever issues fall outside the run it keeps, and that
			// can be either end of an edge — or a holder of a key another
			// issue shares, since the run it keeps ascends strictly. Naming one
			// cause would misreport why the run touched an issue.
			_, err = fmt.Fprintf(w, "Re-ranked %d issue(s): no rank inversion remains, and every issue holds a rank key of its own.\n", fixed)
		}
		return err
	}},
}

// joinCycle renders a cycle field: its members, or "none" for a check that ran
// and found no loop.
func joinCycle(members []string) string {
	if len(members) == 0 {
		return "none"
	}
	return strings.Join(members, "->")
}

// doctorFieldValue renders one field of the status line, substituting
// "unchecked" for any check the report says it did not run.
//
// The status line is parsed by scripts, and `rank_inversions=0` from a check
// that never happened is a false clean claim no exit code can correct — the
// reader has already been told a number. So the absent case gets a value of its
// own rather than sharing one with the clean case. [LAW:no-silent-failure]
// [LAW:one-source-of-truth] Which checks were skipped is the report's own
// statement; this renders it rather than re-deriving it from the faults found.
func doctorFieldValue(report storage.HealthReport, field, value string) string {
	if slices.Contains(report.Unchecked, field) {
		return "unchecked"
	}
	return value
}

// printReportWarnings writes one `warning:` line per warning the health report
// carries. The status line holds only counts, so a finding with no field there
// (a ticket outside an epic) reaches the operator here or nowhere, and a finding
// with one (a dependency cycle) gains the repair the field cannot name.
// [LAW:no-silent-failure]
func printReportWarnings(w io.Writer, warnings []string) error {
	for _, warning := range warnings {
		if _, err := fmt.Fprintf(w, "warning: %s\n", warning); err != nil {
			return err
		}
	}
	return nil
}

// printFootprint writes the store's size on disk against its ceiling, then
// every part of it, so a reader asking where the disk went is answered from
// one line. A part the ceiling does not count says so beside its size. A
// footprint that could not be read says so, and the warnings say why.
func printFootprint(w io.Writer, fp storage.Footprint) error {
	if !fp.Measured() {
		_, err := fmt.Fprintln(w, "footprint: not measured")
		return err
	}
	parts := make([]string, 0, len(fp.Components))
	for _, c := range fp.Components {
		part := c.Name + " " + storage.FormatBytes(c.Bytes)
		if !c.Governed {
			part += " (outside the ceiling)"
		}
		parts = append(parts, part)
	}
	_, err := fmt.Fprintf(w, "footprint: %s of a %s ceiling; %s\n",
		storage.FormatBytes(fp.GovernedBytes()), storage.FormatBytes(fp.Ceiling), strings.Join(parts, ", "))
	return err
}

// footprintFieldValue renders one footprint fact for the event: value when
// the footprint was measured, "unmeasured" when it could not be read, so a
// size never read is not recorded as a store of zero bytes under a zero
// ceiling — the same reason doctorFieldValue spells "unchecked".
// [LAW:no-silent-failure] [LAW:single-enforcer] every footprint fact renders
// through here.
func footprintFieldValue(fp storage.Footprint, value string) string {
	if !fp.Measured() {
		return "unmeasured"
	}
	return value
}

// overCeilingPart names the part holding the bytes of a store over its
// ceiling, or "none" for a store within it, which states a clean check
// rather than leaving an absent value to be read as one.
func overCeilingPart(fp storage.Footprint) string {
	over, ok := fp.Over()
	if !ok {
		return "none"
	}
	return over.Largest.Name
}

// printDoctorReport writes the health report to stdout: the workspace identity,
// the status line and the lines that name its findings, then sync freshness.
func printDoctorReport(ctx context.Context, stdout io.Writer, ap *app.App, report storage.HealthReport, syncReport doctorSyncReport) error {
	census, err := readIDPrefixCensus(ctx, ap.Store)
	if err != nil {
		return err
	}
	if err := printWorkspaceIdentity(stdout, ap.Workspace, census); err != nil {
		return err
	}
	if err := printPrefixMismatch(stdout, ap.Workspace.IssuePrefix, census); err != nil {
		return err
	}
	// [LAW:one-source-of-truth] version.Info is the only source; resolved here
	// (not inside a pure renderer) because it is itself a boundary read.
	if _, err := fmt.Fprintln(stdout, resolveBuildStatusNote(time.Now())); err != nil {
		return err
	}
	dependencyCycle := doctorFieldValue(report, storage.CheckDependencyCycle, joinCycle(report.DependencyCycle))
	// A parent cycle reads on the same line as the dependency cycle because
	// it answers the same shape of question about the other graph, and "none"
	// states a clean check rather than leaving its absence to be read as one.
	parentCycle := doctorFieldValue(report, "parent_cycle", joinCycle(report.ParentCycle))
	rankInversions := doctorFieldValue(report, storage.CheckRankInversions, strconv.Itoa(report.RankInversions))
	duplicateRanks := doctorFieldValue(report, storage.CheckDuplicateRanks, strconv.Itoa(report.DuplicateRanks))
	waitLoopCount, loops, err := doctorWaitLoops(ctx, ap.Store, report)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "integrity_check=%s foreign_key_issues=%d invalid_related_rows=%d orphan_history_rows=%d rank_inversions=%s duplicate_ranks=%s dependency_cycle=%s parent_cycle=%s wait_loops=%s\n", report.IntegrityCheck, report.ForeignKeyIssues, report.InvalidRelatedRows, report.OrphanHistoryRows, rankInversions, duplicateRanks, dependencyCycle, parentCycle, waitLoopCount); err != nil {
		return err
	}
	if err := printFootprint(stdout, report.Footprint); err != nil {
		return err
	}
	if err := printReportWarnings(stdout, report.Warnings); err != nil {
		return err
	}
	if err := printWaitLoops(stdout, loops); err != nil {
		return err
	}
	if err := printSyncFreshness(stdout, syncReport); err != nil {
		return err
	}
	return printPushOutcomeHealth(stdout, ap.Workspace, time.Now(), runningBinaryVersion())
}

// diagnoseThenRepair runs the health check, then such repairs as it is safe to
// run, and reports the state left behind.
//
// The order is the whole point. A repair that climbs the hierarchy starts from
// the live-issue classification, and that read refuses a hierarchy holding a
// loop — so repairing first would make `lit doctor --fix` fail on the repair
// before it could report anything else, and the operator whose habit is
// `--fix` would get no report at all. Diagnosis has to survive the state it
// diagnoses. Store.Doctor answers the same ordering question one level down,
// for the same reason.
//
// A loop withholds only the repairs that would fail on it. Repairs that never
// read the hierarchy still run, because refusing them would strand faults the
// operator can fix in a workspace they cannot yet climb.
// A repair that fails stops the repairs after it, never the report: the state
// the earlier ones left behind is still read and returned beside the failure,
// so a refused --fix costs the operator the repair and not the diagnosis.
// [LAW:dataflow-not-control-flow] What runs is decided by the report and by
// each repair's own declared reach, never by which flag the caller typed.
func diagnoseThenRepair(ctx context.Context, progress io.Writer, repairer storage.Repairer, fixes []doctorFix) (doctorRun, error) {
	report, err := repairer.Doctor(ctx)
	if err != nil {
		return doctorRun{}, err
	}
	if len(report.ParentCycle) > 0 {
		var runnable []doctorFix
		var withheld []string
		for _, fix := range fixes {
			if fix.climbsHierarchy {
				withheld = append(withheld, fix.name)
				continue
			}
			runnable = append(runnable, fix)
		}
		if len(withheld) > 0 {
			// [LAW:no-silent-failure] The skip is stated. A run that printed only
			// the cycle would read as though these repairs had run and found
			// nothing.
			fmt.Fprintf(progress, "doctor: skipping --fix %s because the hierarchy holds a cycle (%s); those repairs start from a read of the hierarchy, which refuses a loop — break it first, then re-run\n", strings.Join(withheld, ","), strings.Join(report.ParentCycle, " -> "))
		}
		fixes = runnable
	}
	if len(fixes) == 0 {
		return doctorRun{report: report}, nil
	}
	var repairErr error
	for _, fix := range fixes {
		if repairErr = fix.run(ctx, progress, repairer); repairErr != nil {
			break
		}
	}
	// Re-read, so the report describes the state the repairs left behind.
	report, err = repairer.Doctor(ctx)
	if err != nil {
		return doctorRun{}, errors.Join(repairErr, err)
	}
	return doctorRun{report: report, repairErr: repairErr}, nil
}

// doctorRun is what diagnoseThenRepair leaves behind: the health report of the
// workspace as the repairs left it, and the error of the repair that failed, if
// one did. The two travel together because the report is owed either way.
type doctorRun struct {
	report    storage.HealthReport
	repairErr error
}

func doctorLeaf() appLeaf {
	fs := newCobraFlagSet("doctor")
	fix := fs.String("fix", "", "Apply fixes: --fix (all) or --fix=rank,thingA")
	fs.cmd.Flags().Lookup("fix").NoOptDefVal = "all"
	return appLeaf{fs: fs, positionals: 0, work: func(ctx context.Context, inv Invocation, ap *app.App, positional []string) error {
		// [LAW:parse-dont-validate] Repair is a capability, not a duty every engine
		// owes: what counts as a fault is engine-specific. Doctor asks once, here,
		// and the fixes below take the interface rather than the app, so no fix can
		// reach an engine that has nothing to repair.
		repairer, err := storage.Repair.Of(ap.Store)
		if err != nil {
			return err
		}
		// [LAW:parse-dont-validate] The requested fixes are resolved to functions
		// before anything runs, so an unknown name is a usage error rather than a
		// failure discovered halfway through repairing.
		var fixes []doctorFix
		if *fix != "" {
			fixNames := allDoctorFixNames()
			if *fix != "all" {
				fixNames = splitCSV(*fix)
			}
			for _, name := range fixNames {
				fix, ok := doctorFixes[name]
				if !ok {
					return fmt.Errorf("unknown fix %q; available: %s", name, strings.Join(allDoctorFixNames(), ", "))
				}
				fix.name = name
				fixes = append(fixes, fix)
			}
		}
		// Fix progress writes to stderr so stdout carries only the health report.
		run, err := diagnoseThenRepair(ctx, os.Stderr, repairer, fixes)
		if err != nil {
			return err
		}
		report := run.report
		// The status line's own rendering, so a check a parent cycle withheld
		// reads "unchecked" here too rather than a clean 0. [LAW:nothing-unseen]
		telemetry.Set(ctx, "doctor.duplicate_ranks", doctorFieldValue(report, storage.CheckDuplicateRanks, strconv.Itoa(report.DuplicateRanks)))
		telemetry.Set(ctx, "doctor.rank_inversions", doctorFieldValue(report, storage.CheckRankInversions, strconv.Itoa(report.RankInversions)))
		telemetry.Set(ctx, "doctor.dependency_cycle", doctorFieldValue(report, storage.CheckDependencyCycle, joinCycle(report.DependencyCycle)))
		telemetry.Set(ctx, "doctor.interleaved", doctorFieldValue(report, storage.CheckRankInversions, strconv.Itoa(len(report.Interleaved))))
		telemetry.Set(ctx, "doctor.footprint_bytes", footprintFieldValue(report.Footprint, strconv.FormatInt(report.Footprint.GovernedBytes(), 10)))
		telemetry.Set(ctx, "doctor.footprint_ceiling", footprintFieldValue(report.Footprint, strconv.FormatInt(report.Footprint.Ceiling, 10)))
		telemetry.Set(ctx, "doctor.over_ceiling", footprintFieldValue(report.Footprint, overCeilingPart(report.Footprint)))
		// [LAW:effects-at-boundaries] Freshness resolution shells out to git and
		// reads the store, so it runs here, at the boundary, before the pure text
		// rendering below.
		syncReport := resolveDoctorSyncFreshness(ctx, inv.Env, ap.Workspace, ap.Store)
		// [LAW:no-silent-failure] A failed repair is joined to any error that cuts
		// the report short, so neither hides the other. A report that prints in full
		// exits on the repair: it is the outcome of the command the operator typed,
		// and it outranks the health exits below as it did when it returned first.
		if err := printDoctorReport(ctx, inv.Stdout, ap, report, syncReport); err != nil || run.repairErr != nil {
			return errors.Join(run.repairErr, err)
		}
		// [LAW:single-enforcer] Corruption classification is output-format agnostic and always enforced here.
		if len(report.Errors) > 0 {
			return CorruptionError{Message: strings.Join(report.Errors, "; "), Cause: parentCycleCause(report)}
		}
		// A divergence that has festered past the persistence threshold surfaces the
		// sync-failure contract on stderr and exits nonzero — the stdout freshness line
		// above stays the routine diagnostic; this is the escalation. Corruption (a
		// harder failure) already returned above, so it wins when both hold.
		// Doctor can be the FIRST detector when auto-sync is disabled, so its
		// escalation feeds the owner channel like every other surface, de-duplicated
		// per episode (links-sync-pgct.4).
		exitErr := doctorDivergenceExit(syncReport)
		var syncFailure SyncFailureError
		if errors.As(exitErr, &syncFailure) {
			if ev, ok := ownerNotifyEventForFailure(syncFailure.Failure); ok {
				maybeNotifyOwner(ctx, inv.Env, ap.Workspace, ev)
			}
		}
		// A workspace that cannot mint is not healthy, so doctor reports the
		// state above and then exits on it — after the owner notification, which
		// a divergence is owed whatever else is wrong. It outranks the divergence
		// exit because it is the one of the two the operator can clear right now.
		if _, err := ap.Workspace.IssuePrefix.Mintable(); err != nil {
			return err
		}
		return exitErr
	}}
}

// parentCycleCause is the typed fault a report that found a parent loop
// carries, or nothing. The loop's repair is not --fix integrity, so the
// report's exit names the loop's own.
func parentCycleCause(report storage.HealthReport) error {
	if len(report.ParentCycle) == 0 {
		return nil
	}
	return storage.ParentCycleError{Cycle: report.ParentCycle}
}
