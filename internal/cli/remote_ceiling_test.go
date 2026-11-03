package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// parsedDowngradeFlags is `lit downgrade`'s flag set after parsing args, as
// the acquisition reads it.
func parsedDowngradeFlags(t *testing.T, args []string) downgradeDeclaration {
	t.Helper()
	d := downgradeLeafWith(&stubResolver{}, &stubInstaller{}, fixedBinPath("/p/lit", nil))
	var out bytes.Buffer
	if _, err := parseLeaf(underInvocation(context.Background()), d.leaf, args, dirInvocation(t, t.TempDir(), &out, &out, nil)); err != nil {
		t.Fatalf("parseLeaf(%q): %v", args, err)
	}
	return d
}

// `lit downgrade` has two forms, chosen by the flags before anything opens:
// --to alone, or --schema with --remote. Every other combination is refused
// as a validation error, and neither remote flag stands alone.
func TestDowngradeFormOfAcceptsExactlyTheTwoForms(t *testing.T) {
	t.Parallel()
	accepted := map[string]downgradeForm{
		"--to v0.4.1":                   downgradeToRelease,
		"--schema 11 --remote origin":   downgradeRemote,
		"--remote origin --schema 11":   downgradeRemote,
		"--schema 11 --remote=upstream": downgradeRemote,
	}
	for args, want := range accepted {
		d := parsedDowngradeFlags(t, strings.Fields(args))
		got, err := downgradeFormOf(d.leaf.fs)
		if err != nil || got != want {
			t.Errorf("downgradeFormOf(%s) = %v, %v; want %v", args, got, err, want)
		}
	}
	for _, args := range []string{"", "--schema 11", "--remote origin", "--to v0.4.1 --remote origin", "--to v0.4.1 --schema 11", "--to v0.4.1 --schema 11 --remote origin"} {
		d := parsedDowngradeFlags(t, strings.Fields(args))
		_, err := downgradeFormOf(d.leaf.fs)
		var refused model.ValidationError
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), "--schema <v> --remote <name>") {
			t.Errorf("downgradeFormOf(%q) = %v, want a validation error naming the two forms", args, err)
		}
	}
}

// The remote form's values are parsed once: a schema version at or above 1
// and a non-empty remote name.
func TestParseRemoteDowngradeRefusesABadSchemaOrRemote(t *testing.T) {
	t.Parallel()
	if got, err := parseRemoteDowngrade(11, " origin "); err != nil || got != (remoteDowngradeRequest{schema: 11, remote: "origin"}) {
		t.Fatalf("parseRemoteDowngrade(11, origin) = %+v, %v", got, err)
	}
	for _, c := range []struct {
		schema int
		remote string
		want   string
	}{
		{0, "origin", "--schema 0 is not a schema version"},
		{-3, "origin", "--schema -3 is not a schema version"},
		{11, "  ", "--remote requires a non-empty remote name"},
	} {
		_, err := parseRemoteDowngrade(c.schema, c.remote)
		var refused model.ValidationError
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("parseRemoteDowngrade(%d, %q) = %v, want a validation error with %q", c.schema, c.remote, err, c.want)
		}
	}
}

// `lit upgrade` lifts a remote's hold with --remote, which installs nothing
// and so does not combine with --to.
func TestUpgradeFormOfRefusesRemoteBesideTo(t *testing.T) {
	t.Parallel()
	parse := func(args []string) upgradeDeclaration {
		d := upgradeLeafWith(&stubResolver{}, &stubInstaller{}, fixedBinPath("/p/lit", nil))
		var out bytes.Buffer
		if _, err := parseLeaf(underInvocation(context.Background()), d.leaf, args, dirInvocation(t, t.TempDir(), &out, &out, nil)); err != nil {
			t.Fatalf("parseLeaf(%q): %v", args, err)
		}
		return d
	}
	for args, want := range map[string]upgradeForm{"": upgradeInstall, "--to v1.2.0": upgradeInstall, "--remote origin": upgradeLiftRemote} {
		got, err := upgradeFormOf(parse(strings.Fields(args)).leaf.fs)
		if err != nil || got != want {
			t.Errorf("upgradeFormOf(%q) = %v, %v; want %v", args, got, err, want)
		}
	}
	_, err := upgradeFormOf(parse([]string{"--to", "v1.2.0", "--remote", "origin"}).leaf.fs)
	var refused model.ValidationError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "installs nothing; give it without --to") {
		t.Fatalf("upgradeFormOf(--to --remote) = %v, want a validation error", err)
	}
	if remote, err := parse([]string{"--remote", " fork "}).remote(); err != nil || remote != "fork" {
		t.Fatalf("remote() = %q, %v; want fork", remote, err)
	}
	if _, err := parse([]string{"--remote", ""}).remote(); err == nil || !strings.Contains(err.Error(), "upgrade: --remote requires a non-empty remote name") {
		t.Fatalf("remote() of an empty --remote = %v, want it refused", err)
	}
}

type stubHolder struct {
	done   *storage.Downgraded
	err    error
	target int64
}

func (s *stubHolder) DowngradeAndHold(_ context.Context, target int64) (*storage.Downgraded, error) {
	s.target = target
	return s.done, s.err
}

type stubLifter struct {
	lifted storage.SchemaHoldLifted
	err    error
	called bool
}

func (s *stubLifter) LiftSchemaHold(context.Context) (storage.SchemaHoldLifted, error) {
	s.called = true
	return s.lifted, s.err
}

// recordedPush is a push dependency that records whether it ran, and what
// had been printed when it did.
type recordedPush struct {
	called     bool
	printedYet string
	out        *bytes.Buffer
}

func (p *recordedPush) push(context.Context, Invocation) error {
	p.called = true
	p.printedYet = p.out.String()
	return nil
}

// runRecorded runs work under a telemetry recorder and returns the facts and
// counts the invocation's event carries, with what was printed.
func runRecorded(t *testing.T, out *bytes.Buffer, work func(ctx context.Context, inv Invocation) error) (map[string]any, map[string]int, error) {
	t.Helper()
	recorder := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(context.Background(), recorder)
	err := work(ctx, dirInvocation(t, t.TempDir(), out, out, nil))
	ev := recorder.Finish(invocationEnd(time.Now(), err, nil))
	return writtenFacts(t, ev), ev.Counts, err
}

// The remote downgrade holds this backlog at the requested schema, says so,
// then pushes; its event carries the remote, the target and the hold.
func TestARemoteDowngradeHoldsThenPushesTheHeldHead(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	holder := &stubHolder{done: &storage.Downgraded{From: 12, Reversed: 1, Hold: 11}}
	push := &recordedPush{out: &out}
	facts, counts, err := runRecorded(t, &out, func(ctx context.Context, inv Invocation) error {
		return remoteDowngradeWork(ctx, inv, remoteDowngradeRequest{schema: 11, remote: "origin"}, remoteDowngradeScope{schema: holder, current: newerCurrentInfo(), push: push.push})
	})
	if err != nil {
		t.Fatalf("remoteDowngradeWork: %v", err)
	}
	if holder.target != 11 {
		t.Errorf("DowngradeAndHold target = %d, want 11", holder.target)
	}
	const line = "lowered this backlog to schema v11 and held it there; pushing the held head to origin\n"
	if !push.called || push.printedYet != line {
		t.Errorf("push called = %v after printing %q; want it called after %q", push.called, push.printedYet, line)
	}
	for key, want := range map[string]any{
		"downgrade.remote":             "origin",
		"downgrade.target_schema_max":  float64(11),
		"downgrade.running_schema_min": float64(1),
		"downgrade.running_schema_max": float64(4),
		"downgrade.workspace_schema":   float64(12),
		"downgrade.schema_hold":        float64(11),
	} {
		if facts[key] != want {
			t.Errorf("%s = %v (%T), want %v", key, facts[key], facts[key], want)
		}
	}
	if counts["downgrade.reversed"] != 1 {
		t.Errorf("downgrade.reversed = %d, want 1", counts["downgrade.reversed"])
	}
	if _, set := facts["downgrade.tag"]; set {
		t.Errorf("downgrade.tag = %v, want it absent: the remote form resolves no release", facts["downgrade.tag"])
	}
}

// The remote downgrade says what it did from the store's report: a re-run
// after a failed push reverses nothing and says the backlog is already held.
func TestARemoteDowngradeSaysWhetherItLoweredAnything(t *testing.T) {
	t.Parallel()
	for reversed, want := range map[int]string{
		1: "lowered this backlog to schema v11 and held it there",
		0: "this backlog is at schema v11 and held there",
	} {
		if got := loweredLine(11, reversed); got != want {
			t.Errorf("loweredLine(11, %d) = %q, want %q", reversed, got, want)
		}
	}
}

// A target above this backlog's schema is refused in the remote form's own
// words: the store's refusal names `lit upgrade`, which installs a release
// and moves this backlog nowhere.
func TestARemoteDowngradeBelowItsTargetNamesHowTheBacklogRises(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	holder := &stubHolder{done: &storage.Downgraded{From: 6}, err: &store.DowngradeTargetAheadError{Current: 6, Target: 7}}
	push := &recordedPush{out: &out}
	_, _, err := runRecorded(t, &out, func(ctx context.Context, inv Invocation) error {
		return remoteDowngradeWork(ctx, inv, remoteDowngradeRequest{schema: 7, remote: "origin"}, remoteDowngradeScope{schema: holder, current: newerCurrentInfo(), push: push.push})
	})
	var refused model.ValidationError
	if !errors.As(err, &refused) || push.called {
		t.Fatalf("remoteDowngradeWork = %v (push called %v), want a validation error and no push", err, push.called)
	}
	for _, want := range []string{"at schema v6, below v7", "`lit ls`", "schema v4", "`lit upgrade --remote origin` lifts a held one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
	if strings.Contains(strings.ReplaceAll(err.Error(), "lit upgrade --remote", ""), "lit upgrade") {
		t.Errorf("refusal names the install: %v", err)
	}
}

// A reverse that fails pushes nothing, and its report still reaches the event.
func TestARemoteDowngradeThatFailsPushesNothing(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	holder := &stubHolder{done: &storage.Downgraded{From: 12}, err: errors.New("schema refused")}
	push := &recordedPush{out: &out}
	facts, _, err := runRecorded(t, &out, func(ctx context.Context, inv Invocation) error {
		return remoteDowngradeWork(ctx, inv, remoteDowngradeRequest{schema: 11, remote: "origin"}, remoteDowngradeScope{schema: holder, current: newerCurrentInfo(), push: push.push})
	})
	if err == nil || !strings.Contains(err.Error(), "schema refused") {
		t.Fatalf("remoteDowngradeWork = %v, want the store's refusal", err)
	}
	if push.called {
		t.Error("the push ran after a failed reverse")
	}
	if facts["downgrade.workspace_schema"] != float64(12) {
		t.Errorf("downgrade.workspace_schema = %v, want 12", facts["downgrade.workspace_schema"])
	}
}

// The lift removes the hold, says what it lifted and how the remote rises,
// then pushes; a backlog with no hold says so and still pushes.
func TestALiftPushesTheLiftedHead(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		lifted int64
		line   string
	}{
		{11, "lifted this backlog's schema hold at v11; pushing to origin\n"},
		{0, "this backlog holds no schema hold; pushing to origin\n"},
	} {
		var out bytes.Buffer
		lifter := &stubLifter{lifted: storage.SchemaHoldLifted{Lifted: c.lifted}}
		push := &recordedPush{out: &out}
		facts, _, err := runRecorded(t, &out, func(ctx context.Context, inv Invocation) error {
			return liftRemoteHoldWork(ctx, inv, "origin", liftScope{holder: lifter, push: push.push})
		})
		if err != nil {
			t.Fatalf("liftRemoteHoldWork: %v", err)
		}
		want := c.line + liftRiseLine + "\n"
		if !lifter.called || !push.called || push.printedYet != want {
			t.Errorf("lifted %d: lifter %v, push %v after printing %q; want %q", c.lifted, lifter.called, push.called, push.printedYet, want)
		}
		if facts["upgrade.remote"] != "origin" || facts["schema_hold.lifted"] != float64(c.lifted) {
			t.Errorf("lifted %d: facts upgrade.remote = %v, schema_hold.lifted = %v", c.lifted, facts["upgrade.remote"], facts["schema_hold.lifted"])
		}
	}
	var out bytes.Buffer
	push := &recordedPush{out: &out}
	_, _, err := runRecorded(t, &out, func(ctx context.Context, inv Invocation) error {
		return liftRemoteHoldWork(ctx, inv, "origin", liftScope{holder: &stubLifter{err: errors.New("meta schema_hold = \"x\" is not a schema version")}, push: push.push})
	})
	if err == nil || push.called {
		t.Fatalf("a failed lift: err = %v, push called = %v; want the error and no push", err, push.called)
	}
}

// asSyncFailure routes the store's lowering refusal into the sync-failure
// contract: the pull first, since a backlog that is only behind reads the
// same as one that stepped down, and the remote downgrade last, as the
// user's decision.
func TestALoweringRefusalRendersTheSyncFailureContract(t *testing.T) {
	t.Parallel()
	storeErr := &store.SchemaLoweringRequiresHoldError{Remote: "origin", Branch: "master", RemoteSchema: 12, CommitSchema: 11}
	var syncFailure SyncFailureError
	if !errors.As(asSyncFailure(storeErr), &syncFailure) {
		t.Fatalf("asSyncFailure(%v) is not a SyncFailureError", storeErr)
	}
	if syncFailure.Failure.Class != syncFailureSchemaLoweringRequiresHold || syncFailure.Failure.LoweringRefused != storeErr {
		t.Fatalf("mapped failure = %+v", syncFailure.Failure)
	}
	var unwrapped *store.SchemaLoweringRequiresHoldError
	if !errors.As(syncFailure, &unwrapped) || unwrapped != storeErr {
		t.Fatalf("the sync failure does not unwrap to the store refusal: %v", syncFailure)
	}
	if mirror, ok := schemaRefusalFailure(storeErr); !ok || mirror.Class != syncFailureSchemaLoweringRequiresHold {
		t.Fatalf("the mirror's schema-refusal adapter does not recognize the lowering: %+v %v", mirror, ok)
	}
	block := syncFailure.Error()
	for _, want := range []string{
		"would lower the remote's schema with a head that holds no schema hold",
		"is at schema version 11, below the schema version 12 origin/master was at when last fetched",
		"lit sync pull --remote origin",
		"lit downgrade --schema 11 --remote origin",
		"only when the user means to lower origin",
		"ESCALATION — resolve now",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block lacks %q:\n%s", want, block)
		}
	}
	if pull, downgrade := strings.Index(block, "lit sync pull"), strings.Index(block, "lit downgrade --schema"); pull > downgrade {
		t.Errorf("the pull comes after the remote downgrade in the remedy:\n%s", block)
	}
}

// A remote lowered by `lit downgrade --remote` stays lowered: a second client
// at this tree's schema pulls the held head and is refused the raise, the
// remote keeps its head, and `lit upgrade --remote` lets it rise — in two
// pushes, the lift's and a release's.
func TestARemoteLoweredByDowngradeStaysLoweredUntilLifted(t *testing.T) {
	remote, producer, consumer, _, epicID := twoClonesOverOneDoltRemote(t)
	max, err := migrations.MaxVersion()
	if err != nil {
		t.Fatalf("MaxVersion: %v", err)
	}
	target := strconv.FormatInt(max-1, 10)
	before := remoteDoltHead(t, remote)

	// A remote with nothing on it refuses the lowering before the store is
	// touched: the lowering that follows still has a migration to reverse.
	runGit(t, filepath.Dir(remote), "init", "--bare", "empty.git")
	runGit(t, producer, "remote", "add", "empty", filepath.Join(filepath.Dir(remote), "empty.git"))
	refusedOut, err := runCLIInDirAllowError(t, producer, "downgrade", "--schema", target, "--remote", "empty")
	if err == nil || !strings.Contains(err.Error()+refusedOut, "lit downgrade --schema "+target+": empty has no backlog to push to (remote_empty)") {
		t.Fatalf("downgrade --remote empty: err = %v, want the empty remote refused before the reverse\noutput:\n%s", err, refusedOut)
	}

	// A remote holding a peer's commit this backlog lacks refuses the
	// lowering before the store is touched, naming the pull.
	runCLIInDir(t, consumer, "new", "--title", "peer-ticket", "--topic", "demo", "--type", "task", "--parent", epicID)
	runCLIInDir(t, consumer, "sync", "push")
	refusedOut, err = runCLIInDirAllowError(t, producer, "downgrade", "--schema", target, "--remote", "origin")
	if err == nil || !strings.Contains(err.Error()+refusedOut, "origin/master holds 1 commit(s) this backlog lacks, so the push would be refused as diverged; run `lit sync pull --remote origin` first") {
		t.Fatalf("downgrade --remote over a peer's push: err = %v, want the pull named before the reverse\noutput:\n%s", err, refusedOut)
	}
	runCLIInDir(t, producer, "sync", "pull")
	before = remoteDoltHead(t, remote)

	out := runCLIInDir(t, producer, "downgrade", "--schema", target, "--remote", "origin")
	if want := "lowered this backlog to schema v" + target + " and held it there; pushing the held head to origin"; !strings.Contains(out, want) {
		t.Fatalf("downgrade --remote output lacks %q:\n%s", want, out)
	}
	lowered := remoteDoltHead(t, remote)
	if lowered == before {
		t.Fatalf("the remote head did not move for the lowering push:\n%s", out)
	}

	// The second client pulls the held head: its backlog steps down with it,
	// and the hold then refuses the migrating open that would raise it.
	runCLIInDir(t, consumer, "sync", "pull")
	newOut, err := runCLIInDirAllowError(t, consumer, "new", "--title", "under-the-hold", "--topic", "demo", "--type", "task", "--parent", epicID)
	if err == nil || !strings.Contains(err.Error()+newOut, "meta.schema_hold") {
		t.Fatalf("lit new on a backlog held at %s: err = %v, want the hold refusing the raise\noutput:\n%s", target, err, newOut)
	}
	if got := remoteDoltHead(t, remote); got != lowered {
		t.Fatalf("remote head after the second client's sync = %s, want %s held", got, lowered)
	}

	out = runCLIInDir(t, producer, "upgrade", "--remote", "origin")
	if want := "lifted this backlog's schema hold at v" + target + "; pushing to origin"; !strings.Contains(out, want) {
		t.Fatalf("upgrade --remote output lacks %q:\n%s", want, out)
	}
	liftedHead := remoteDoltHead(t, remote)
	if liftedHead == lowered {
		t.Fatalf("the remote head did not move for the lift push:\n%s", out)
	}
	// The migrating open brings the producer's backlog back up, and the
	// release's push raises the remote.
	runCLIInDir(t, producer, "ls")
	runCLIInDir(t, producer, "sync", "push")
	if got := remoteDoltHead(t, remote); got == liftedHead {
		t.Fatalf("the remote head did not move for the raise")
	}
	runCLIInDir(t, consumer, "sync", "pull")
	runCLIInDir(t, consumer, "new", "--title", "after-the-lift", "--topic", "demo", "--type", "task", "--parent", epicID)
}
