package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// everyReceiveStep is the closed set, in the order the receive runs it.
var everyReceiveStep = []receiveStep{
	receiveStepNone, receiveStepFetch, receiveStepLand, receiveStepOpen,
	receiveStepLiveRemotes, receiveStepSettle, receiveStepReconcile,
}

// TestEveryReadingOfAnOutcomeIsWhereItStopped pins the three readings of how
// far a receive got against every step it can stop at. The settle reported
// ahead, so a receive that got through the settle owes a push. Expected values
// are written out per step, not derived from the order the code compares on.
func TestEveryReadingOfAnOutcomeIsWhereItStopped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		stopped     receiveStep
		fetched     bool
		access      app.AccessMode
		settledOnly bool
	}{
		{receiveStepNone, true, app.AccessWrite, true},
		{receiveStepFetch, false, app.AccessRead, false},
		{receiveStepLand, true, app.AccessRead, false},
		{receiveStepOpen, true, app.AccessRead, false},
		{receiveStepLiveRemotes, true, app.AccessRead, false},
		{receiveStepSettle, true, app.AccessRead, false},
		{receiveStepReconcile, true, app.AccessWrite, false},
	}
	if len(cases) != len(everyReceiveStep) {
		t.Fatalf("%d cases for %d steps; every step needs a row", len(cases), len(everyReceiveStep))
	}
	for _, tc := range cases {
		o := syncReceiveOutcome{state: storage.SyncReceiveAhead, stopped: tc.stopped}
		if tc.stopped != receiveStepNone {
			o.err = errors.New("stopped")
		}
		if got := o.fetched(); got != tc.fetched {
			t.Errorf("stopped at %q: fetched = %v, want %v", tc.stopped, got, tc.fetched)
		}
		if got := o.access(); got != tc.access {
			t.Errorf("stopped at %q: access = %v, want %v", tc.stopped, got, tc.access)
		}
		if got := o.settledCleanly(); got != tc.settledOnly {
			t.Errorf("stopped at %q: settledCleanly = %v, want %v", tc.stopped, got, tc.settledOnly)
		}
	}
}

// TestOnlyALastingRunOfPostFetchStopsIsUnsettled is the two-sided table for
// unsettledLasting: every step, both kinds of cause, and a run that is absent,
// younger than the threshold, exactly at it, and past it. Only a post-fetch
// stop (land, open, live remotes, settle) with a cause other than a
// cancellation, in a run at least unsettledLastingAfter old, is lasting.
func TestOnlyALastingRunOfPostFetchStopsIsUnsettled(t *testing.T) {
	t.Parallel()
	now := time.Now()
	postFetch := map[receiveStep]bool{receiveStepLand: true, receiveStepOpen: true, receiveStepLiveRemotes: true, receiveStepSettle: true}
	causes := map[string]error{"plain": errors.New("held"), "canceled": context.Canceled, "wrapped canceled": errors.Join(errors.New("cut"), context.Canceled)}
	runs := map[string]struct {
		since  time.Time
		lasted bool
	}{
		"no run":         {time.Time{}, false},
		"younger":        {now.Add(-unsettledLastingAfter + time.Second), false},
		"exactly at":     {now.Add(-unsettledLastingAfter), true},
		"past threshold": {now.Add(-unsettledLastingAfter - time.Hour), true},
	}
	for _, step := range everyReceiveStep {
		for causeName, cause := range causes {
			for runName, run := range runs {
				o := syncReceiveOutcome{stopped: step, err: cause, unsettledSince: run.since}
				want := postFetch[step] && causeName == "plain" && run.lasted
				if got := o.unsettledLasting(now); got != want {
					t.Errorf("stopped at %q, %s cause, %s: unsettledLasting = %v, want %v", step, causeName, runName, got, want)
				}
			}
		}
	}
}

// TestTheAutomaticReceiveOwnsTheRun pins ownUnsettledRun's writer rules
// through the records it leaves: a stop that counts starts a run when none is
// current and extends the current one otherwise; a settle that came after a
// run began ends it, so the next stop starts a new run; and a receive that
// stops at the fetch, the reconcile, by cancellation, or not at all writes
// nothing.
func TestTheAutomaticReceiveOwnsTheRun(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Second)
	t0 := now.Add(-time.Hour)
	t1 := now.Add(-30 * time.Minute)
	settleStop := syncReceiveOutcome{stopped: receiveStepSettle, err: errors.New("follow")}
	cases := []struct {
		name      string
		first     time.Time // zero: no record
		settled   time.Time // zero: no record
		outcome   syncReceiveOutcome
		wantSince time.Time
		wantFirst time.Time // what unsettled.first holds afterwards; zero: none
	}{
		{"first stop starts a run", time.Time{}, time.Time{}, settleStop, now, now},
		{"a later stop extends the run", t0, time.Time{}, settleStop, t0, t0},
		{"a run a settle has not ended goes on", t1, t0, settleStop, t1, t1},
		{"a settle after the run began ends it; the stop starts a new one", t0, t1, settleStop, now, now},
		{"a settle that finished after this receive read its clock ends the run it starts", time.Time{}, now.Add(time.Second), settleStop, time.Time{}, now},
		{"a landing stop counts", time.Time{}, t0, syncReceiveOutcome{stopped: receiveStepLand, err: errors.New("land")}, now, now},
		{"a fetch stop does not count", time.Time{}, time.Time{}, syncReceiveOutcome{stopped: receiveStepFetch, err: errors.New("net")}, time.Time{}, time.Time{}},
		{"a reconcile stop does not count", time.Time{}, time.Time{}, syncReceiveOutcome{stopped: receiveStepReconcile, err: errors.New("gc")}, time.Time{}, time.Time{}},
		{"a cancellation does not count", time.Time{}, time.Time{}, syncReceiveOutcome{stopped: receiveStepSettle, err: context.Canceled}, time.Time{}, time.Time{}},
		{"a receive that ran to the end writes nothing", time.Time{}, time.Time{}, syncReceiveOutcome{}, time.Time{}, time.Time{}},
		{"a fetch stop inside a run reports the run", t1, t0, syncReceiveOutcome{stopped: receiveStepFetch, err: errors.New("net")}, t1, t1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			databasePath := filepath.Join(t.TempDir(), "dolt")
			if !tc.first.IsZero() {
				if err := store.WriteUnsettledFirst(databasePath, tc.first); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.settled.IsZero() {
				if err := os.WriteFile(store.SettledLastPath(databasePath), []byte(tc.settled.UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			obs := ownUnsettledRun(databasePath)(tc.outcome, now)
			if obs.err != nil || !obs.since.Equal(tc.wantSince) || !obs.settledLast.Equal(tc.settled) {
				t.Fatalf("observation = %+v; want since %s, settled %s, no error", obs, tc.wantSince, tc.settled)
			}
			first, ok, err := store.ReadUnsettledFirst(databasePath)
			if err != nil || ok != !tc.wantFirst.IsZero() || !first.Equal(tc.wantFirst) {
				t.Fatalf("unsettled.first afterwards = %s ok %v err %v; want %s", first, ok, err, tc.wantFirst)
			}
		})
	}
}

// TestARunRecordThatCannotBeWrittenSaysSo: the run's records sit in the
// storage dir, and a receive that cannot read or write them reports it on the
// observation rather than dropping the run. [LAW:no-silent-failure]
func TestARunRecordThatCannotBeWrittenSaysSo(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "dolt")
	if err := os.MkdirAll(store.UnsettledFirstPath(databasePath), 0o755); err != nil {
		t.Fatal(err)
	}
	obs := ownUnsettledRun(databasePath)(syncReceiveOutcome{stopped: receiveStepSettle, err: errors.New("follow")}, time.Now())
	if obs.err == nil || !strings.Contains(obs.err.Error(), "unsettled.first") {
		t.Fatalf("observation error = %v; want the unsettled.first failure", obs.err)
	}
}

// TestTheMirrorReadsTheRunAndWritesNothing: the mirror's receive does not
// hold the receive lock, so it never writes the run, but it reads both
// records, so its trace and event carry the times as they stand, not zeros
// that read as "never settled". A stop that would start a run with none
// current starts none.
func TestTheMirrorReadsTheRunAndWritesNothing(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Second)
	t0 := now.Add(-time.Hour)
	t1 := now.Add(-30 * time.Minute)
	stop := syncReceiveOutcome{stopped: receiveStepSettle, err: errors.New("follow")}
	cases := []struct {
		name      string
		first     time.Time // zero: no record
		settled   time.Time // zero: no record
		wantSince time.Time
	}{
		{"no records", time.Time{}, time.Time{}, time.Time{}},
		{"a current run", t1, t0, t1},
		{"a run a settle ended", t0, t1, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			databasePath := filepath.Join(t.TempDir(), "dolt")
			if !tc.first.IsZero() {
				if err := store.WriteUnsettledFirst(databasePath, tc.first); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.settled.IsZero() {
				if err := os.WriteFile(store.SettledLastPath(databasePath), []byte(tc.settled.UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			obs := leaveUnsettledRun(databasePath)(stop, now)
			if obs.err != nil || !obs.since.Equal(tc.wantSince) || !obs.settledLast.Equal(tc.settled) {
				t.Fatalf("observation = %+v; want since %s, settled %s, no error", obs, tc.wantSince, tc.settled)
			}
			first, ok, err := store.ReadUnsettledFirst(databasePath)
			if err != nil || ok != !tc.first.IsZero() || !first.Equal(tc.first) {
				t.Fatalf("unsettled.first afterwards = %s ok %v err %v; want %s untouched", first, ok, err, tc.first)
			}
		})
	}
}

// TestEveryStopMapsToItsSyncFailure maps each step against a run below and
// past the threshold. A stop at the reconcile is a divergence the reconcile
// could not resolve, whatever the run; a post-fetch stop is an unsettled
// receive only once its run has lasted; a fetch stop and a receive that ran
// to the end surface nothing; and a cause that is the remote's schema running
// ahead is its own class at any step, on the first stop.
func TestEveryStopMapsToItsSyncFailure(t *testing.T) {
	t.Parallel()
	now := time.Now()
	young, old := now.Add(-time.Minute), now.Add(-unsettledLastingAfter-time.Minute)
	cases := []struct {
		stopped   receiveStep
		since     time.Time
		wantClass syncFailureClass // empty: no block
	}{
		{receiveStepNone, old, ""},
		{receiveStepFetch, old, ""},
		{receiveStepLand, young, ""},
		{receiveStepLand, old, syncFailureReceiveUnsettled},
		{receiveStepOpen, young, ""},
		{receiveStepOpen, old, syncFailureReceiveUnsettled},
		{receiveStepLiveRemotes, young, ""},
		{receiveStepLiveRemotes, old, syncFailureReceiveUnsettled},
		{receiveStepSettle, young, ""},
		{receiveStepSettle, old, syncFailureReceiveUnsettled},
		{receiveStepReconcile, young, syncFailureDivergedUnresolved},
		{receiveStepReconcile, old, syncFailureDivergedUnresolved},
	}
	for _, tc := range cases {
		o := syncReceiveOutcome{remote: remoteName(t, "origin"), branch: "master", stopped: tc.stopped, unsettledSince: tc.since}
		if tc.stopped != receiveStepNone {
			o.err = errors.New("cause-" + tc.stopped.String())
		}
		failure, ok := o.inlineSyncFailure(now)
		if got := map[bool]syncFailureClass{true: failure.Class}[ok]; got != tc.wantClass {
			t.Errorf("stopped at %q, run since %s: class = %q, want %q", tc.stopped, now.Sub(tc.since), got, tc.wantClass)
			continue
		}
		if !ok {
			continue
		}
		if failure.Cause == nil || failure.Cause.Error() != "cause-"+tc.stopped.String() {
			t.Errorf("stopped at %q: cause = %v, want the step's error", tc.stopped, failure.Cause)
		}
		if tc.wantClass == syncFailureReceiveUnsettled &&
			(failure.StoppedAt != tc.stopped || !failure.UnsettledSince.Equal(tc.since) || failure.Age != now.Sub(tc.since)) {
			t.Errorf("stopped at %q: unsettled failure = %+v; want the step, the run's start and its age", tc.stopped, failure)
		}
	}
	for _, step := range []receiveStep{receiveStepSettle, receiveStepReconcile} {
		ahead := &store.RemoteSchemaAheadError{Remote: "origin", Branch: "master", RemoteVersion: 9, BinarySupportedMax: 8}
		o := syncReceiveOutcome{remote: remoteName(t, "origin"), branch: "master", stopped: step, err: ahead}
		if failure, ok := o.inlineSyncFailure(now); !ok || failure.Class != syncFailureRemoteSchemaAhead {
			t.Errorf("schema-ahead cause at %q on the first stop = %q (ok %v); want the schema-ahead class", step, failure.Class, ok)
		}
	}
}

// TestEveryClassRendersItsOwnBlock renders each class in the closed set and
// checks that no part of its block falls to the "unrecognized class" arm,
// so a class added without its headline, escalation, what-line or remedy
// fails here and not in a release.
func TestEveryClassRendersItsOwnBlock(t *testing.T) {
	t.Parallel()
	classes := []syncFailureClass{
		syncFailureProseHeld, syncFailureDivergedUnresolved, syncFailureRemoteSchemaAhead,
		syncFailureUnrelatedHistories, syncFailureIDCollision, syncFailureReceiveUnsettled,
	}
	for _, class := range classes {
		failure := SyncFailure{
			Class: class, Remote: "origin", Branch: "master",
			RemoteAhead:    &store.RemoteSchemaAheadError{Remote: "origin", Branch: "master", RemoteVersion: 9, BinarySupportedMax: 8},
			StoppedAt:      receiveStepSettle,
			UnsettledSince: time.Now().Add(-time.Hour),
			Age:            time.Hour,
		}
		if block := failure.blockString(); strings.Contains(block, "unrecognized") {
			t.Errorf("class %q renders an unrecognized-class arm:\n%s", class, block)
		}
	}
	unsettled := SyncFailure{Class: syncFailureReceiveUnsettled, Remote: "origin", Branch: "master", StoppedAt: receiveStepLand,
		UnsettledSince: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), Age: 45 * time.Minute}
	block := unsettled.blockString()
	for _, want := range []string{
		"lit's automatic receive fetched the remote but could not bring it into this checkout, and needs you.",
		"the automatic receive fetched origin/master and then stopped at landing the fetch on the live store. A receive first stopped after its fetch at 2026-10-20T09:00:00Z, and no settle has succeeded on this checkout since then",
		"ESCALATION — BLOCKED: no settle has succeeded on this checkout since a receive first stopped after its fetch at 2026-10-20T09:00:00Z (45 minutes), and the latest receive stopped at landing the fetch on the live store;",
		"lit sync pull             # fetches and settles in the foreground",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("receive_unsettled block missing %q:\n%s", want, block)
		}
	}
	// A remote schema ahead of this binary reaches the block from any step,
	// the fetch and the settle included, so its headline names the upgrade,
	// not a divergence.
	schemaAhead := SyncFailure{Class: syncFailureRemoteSchemaAhead, Remote: "origin", Branch: "master",
		RemoteAhead: &store.RemoteSchemaAheadError{Remote: "origin", Branch: "master", RemoteVersion: 9, BinarySupportedMax: 8}}
	if want := "lit sync cannot write to the remote's backlog until this lit is upgraded, and needs you.\n"; !strings.HasPrefix(schemaAhead.blockString(), agentInstructionsOpen+"\n"+want) {
		t.Errorf("remote_schema_ahead block does not open with %q:\n%s", want, schemaAhead.blockString())
	}
}

// TestAnUnsettledRunIsOneEpisode pins the receive_unsettled fingerprint: the
// run is the episode, so stops at different steps in one run are one episode,
// a new run is a new one and pages on first detection even under a fresh
// marker the old run left, and the sync target does not split it.
func TestAnUnsettledRunIsOneEpisode(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	event := func(since time.Time, step receiveStep, remote string) ownerNotifyEvent {
		ev, ok := ownerNotifyEventForFailure(SyncFailure{Class: syncFailureReceiveUnsettled, Remote: remote, Branch: "master", StoppedAt: step, UnsettledSince: since})
		if !ok || ev.Kind != ownerNotifyKind(syncFailureReceiveUnsettled) || !ev.EpisodeStart.Equal(since) {
			t.Fatalf("ownerNotifyEventForFailure = %+v (ok %v); want a receive_unsettled event for the run at %s", ev, ok, since)
		}
		return ev
	}
	run := event(t0, receiveStepLand, "origin").fingerprint()
	if same := event(t0, receiveStepSettle, "upstream").fingerprint(); same != run {
		t.Fatalf("one run read as two episodes: %q vs %q", run, same)
	}
	next := event(t0.Add(2*time.Hour), receiveStepSettle, "origin").fingerprint()
	if next == run {
		t.Fatalf("a new run kept the old run's fingerprint %q", run)
	}
	marker := filepath.Join(t.TempDir(), "owner-notify.receive_unsettled.last")
	if err := os.WriteFile(marker, []byte(run+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(3 * time.Hour)
	if ownerNotifyDue(marker, run, now) {
		t.Fatal("the paged run paged again inside its cooldown")
	}
	if !ownerNotifyDue(marker, next, now) {
		t.Fatal("a new run did not page on its first detection")
	}
}
