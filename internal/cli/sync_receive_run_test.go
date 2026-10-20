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
// unsettledLasting: every step, both kinds of cause, a run that is absent,
// younger than the threshold, exactly at it, and past it, and a previous stop
// that is absent, the run's first, or between the first and this one. Only a
// post-fetch stop (land, open, live remotes, settle) with a cause other than a
// cancellation, in a run at least unsettledLastingAfter old with a stop
// between its first and this one, is lasting.
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
	previous := map[string]struct {
		at       func(since time.Time) time.Time
		repeated bool
	}{
		"no previous stop":           {func(time.Time) time.Time { return time.Time{} }, false},
		"previous stop is the first": {func(since time.Time) time.Time { return since }, false},
		"a stop between":             {func(since time.Time) time.Time { return since.Add(time.Minute) }, true},
	}
	for _, step := range everyReceiveStep {
		for causeName, cause := range causes {
			for runName, run := range runs {
				for previousName, prev := range previous {
					o := syncReceiveOutcome{stopped: step, err: cause, unsettledSince: run.since, unsettledPreviousStop: prev.at(run.since)}
					want := postFetch[step] && causeName == "plain" && run.lasted && prev.repeated
					if got := o.unsettledLasting(now); got != want {
						t.Errorf("stopped at %q, %s cause, %s, %s: unsettledLasting = %v, want %v", step, causeName, runName, previousName, got, want)
					}
				}
			}
		}
	}
}

// TestTheAutomaticReceiveOwnsTheRun pins ownUnsettledRun's writer rules
// through the records it leaves: a stop that counts records itself in
// unsettled.last, starts a run when none is current and extends the current
// one otherwise, and reports the latest stop before it in that run; a settle
// that came after a run began ends it, so the next stop starts a new run with
// no previous stop; and a receive that stops at the fetch, the reconcile, by
// cancellation, or not at all writes nothing.
func TestTheAutomaticReceiveOwnsTheRun(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Second)
	t0 := now.Add(-time.Hour)
	t1 := now.Add(-30 * time.Minute)
	settleStop := syncReceiveOutcome{stopped: receiveStepSettle, err: errors.New("follow")}
	none := time.Time{}
	type records struct{ first, last, settled time.Time } // zero: no record
	cases := []struct {
		name         string
		before       records
		outcome      syncReceiveOutcome
		wantSince    time.Time
		wantPrevious time.Time
		after        records
	}{
		{"first stop starts a run", records{}, settleStop, now, none, records{now, now, none}},
		{"a second stop extends the run; its previous stop is the first", records{t0, t0, none}, settleStop, t0, t0, records{t0, now, none}},
		{"a later stop reports the stop between", records{t0, t1, none}, settleStop, t0, t1, records{t0, now, none}},
		{"a run a settle has not ended goes on", records{t1, t1, t0}, settleStop, t1, t1, records{t1, now, t0}},
		{"a settle after the run began ends it; the stop starts a new one", records{t0, t0, t1}, settleStop, now, none, records{now, now, t1}},
		{"a settle that finished after this receive read its clock ends the run it starts", records{settled: now.Add(time.Second)}, settleStop, none, none, records{now, now, now.Add(time.Second)}},
		{"a landing stop counts", records{settled: t0}, syncReceiveOutcome{stopped: receiveStepLand, err: errors.New("land")}, now, none, records{now, now, t0}},
		{"a fetch stop does not count", records{}, syncReceiveOutcome{stopped: receiveStepFetch, err: errors.New("net")}, none, none, records{}},
		{"a reconcile stop does not count", records{}, syncReceiveOutcome{stopped: receiveStepReconcile, err: errors.New("gc")}, none, none, records{}},
		{"a cancellation does not count", records{}, syncReceiveOutcome{stopped: receiveStepSettle, err: context.Canceled}, none, none, records{}},
		{"a receive that ran to the end writes nothing", records{}, syncReceiveOutcome{}, none, none, records{}},
		{"a fetch stop inside a run reports the run and writes nothing", records{t1, t1, t0}, syncReceiveOutcome{stopped: receiveStepFetch, err: errors.New("net")}, t1, t1, records{t1, t1, t0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			databasePath := filepath.Join(t.TempDir(), "dolt")
			seedUnsettledRecords(t, databasePath, tc.before.first, tc.before.last, tc.before.settled)
			obs := ownUnsettledRun(databasePath)(tc.outcome, now)
			if obs.err != nil || !obs.since.Equal(tc.wantSince) || !obs.previousStop.Equal(tc.wantPrevious) || !obs.settledLast.Equal(tc.before.settled) {
				t.Fatalf("observation = %+v; want since %s, previous stop %s, settled %s, no error", obs, tc.wantSince, tc.wantPrevious, tc.before.settled)
			}
			for name, read := range map[string]func(string) (time.Time, bool, error){
				"unsettled.first": store.ReadUnsettledFirst, "unsettled.last": store.ReadUnsettledLast, "settled.last": store.ReadSettledLast,
			} {
				want := map[string]time.Time{"unsettled.first": tc.after.first, "unsettled.last": tc.after.last, "settled.last": tc.after.settled}[name]
				at, ok, err := read(databasePath)
				if err != nil || ok != !want.IsZero() || !at.Equal(want) {
					t.Errorf("%s afterwards = %s ok %v err %v; want %s", name, at, ok, err, want)
				}
			}
		})
	}
}

// seedUnsettledRecords writes each of the run's records that is not zero.
// settled.last has no exported writer, since only the settle writes it, so
// the test writes the file the way the store does.
func seedUnsettledRecords(t *testing.T, databasePath string, first, last, settled time.Time) {
	t.Helper()
	if !first.IsZero() {
		if err := store.WriteUnsettledFirst(databasePath, first); err != nil {
			t.Fatal(err)
		}
	}
	if !last.IsZero() {
		if err := store.WriteUnsettledLast(databasePath, last); err != nil {
			t.Fatal(err)
		}
	}
	if !settled.IsZero() {
		if err := os.MkdirAll(filepath.Dir(store.SettledLastPath(databasePath)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(store.SettledLastPath(databasePath), []byte(settled.UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
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
// hold the receive lock, so it never writes the run, but it reads the
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
		name                    string
		first, last, settled    time.Time // zero: no record
		wantSince, wantPrevious time.Time
	}{
		{"no records", time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}},
		{"a current run", t0, t1, time.Time{}, t0, t1},
		{"a run a settle ended", t0, t0, t1, time.Time{}, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			databasePath := filepath.Join(t.TempDir(), "dolt")
			seedUnsettledRecords(t, databasePath, tc.first, tc.last, tc.settled)
			obs := leaveUnsettledRun(databasePath)(stop, now)
			if obs.err != nil || !obs.since.Equal(tc.wantSince) || !obs.previousStop.Equal(tc.wantPrevious) || !obs.settledLast.Equal(tc.settled) {
				t.Fatalf("observation = %+v; want since %s, previous stop %s, settled %s, no error", obs, tc.wantSince, tc.wantPrevious, tc.settled)
			}
			first, okFirst, errFirst := store.ReadUnsettledFirst(databasePath)
			last, okLast, errLast := store.ReadUnsettledLast(databasePath)
			if errFirst != nil || errLast != nil || okFirst != !tc.first.IsZero() || !first.Equal(tc.first) || okLast != !tc.last.IsZero() || !last.Equal(tc.last) {
				t.Fatalf("records afterwards: first %s (%v, %v), last %s (%v, %v); want %s and %s untouched", first, okFirst, errFirst, last, okLast, errLast, tc.first, tc.last)
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
		o := syncReceiveOutcome{remote: remoteName(t, "origin"), branch: "master", stopped: tc.stopped, unsettledSince: tc.since, unsettledPreviousStop: tc.since.Add(time.Minute)}
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
