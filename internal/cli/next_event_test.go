package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// Every reach the event records has its own name: each kind, and the two lists
// that are not kinds. A kind left unnamed would record a blocker with no reach
// at all, and a shared name would record two answers as one.
func TestEveryReachKindHasAnEventName(t *testing.T) {
	seen := map[string]string{}
	name := func(what, reach string) {
		if reach == "" {
			t.Errorf("%s has no event name", what)
		}
		if other, dup := seen[reach]; dup {
			t.Errorf("%s and %s share the event name %q", other, what, reach)
		}
		seen[reach] = what
	}
	for kind, reach := range reachKindNames {
		name(fmt.Sprintf("reach kind %d", kind), reach)
	}
	name("an out-of-view blocker", outOfViewReach)
	name("an off-path row", offFocusPathReach)
}

// writtenFacts is the event's facts as the events file holds them.
func writtenFacts(t *testing.T, ev telemetry.Event) map[string]any {
	t.Helper()
	line, err := telemetry.Line(ev)
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Facts map[string]any `json:"facts"`
	}
	if err := json.Unmarshal(line, &written); err != nil {
		t.Fatal(err)
	}
	return written.Facts
}

func decodeJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func rowFor(id string) annotation.AnnotatedIssue {
	return annotation.AnnotatedIssue{Issue: model.Issue{ID: id}}
}

// readThrough is a dependency the run read through the gathered ticket under it.
func readThrough(id, ticket string, kind reachKind) depReach {
	return depReach{ID: id, Best: &reading{Row: rowFor(ticket), Kind: kind}}
}

// An exhausted scope's event says how far the scope had come, names every
// blocker and held row with what this checkout may do about each, whether a
// ticket filed here could free each blocker, and whether the route that stays
// in the scope was offered — it is, exactly when one could. A pick served past
// the scope records the same, since it prints the same.
func TestNextRecordsWhatAnExhaustedScopeNamed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outcome  NextOutcome
		gathered int
		want     string
	}{
		{
			name: "a blocker another checkout holds can be cleared from here",
			outcome: Exhausted{
				Epics:    []string{"test-epic"},
				Progress: claims.Progress{Done: 3, Total: 5},
				Blocked: []blockerReach{
					{depReach: readThrough("test-dep", "test-dep-leaf", reachHeldFresh), Clearable: true},
					{depReach: readThrough("test-ext", "test-ext", reachAwaitingOutside)},
				},
				Held: []rowReach{{ID: "test-held", reading: reading{Row: rowFor("test-held"), Kind: reachNotReady}}},
			},
			gathered: 5,
			want: `{
				"next.outcome": "exhausted",
				"next.picked": null,
				"next.dependency_path": [],
				"next.exhausted_epics": ["test-epic"],
				"next.exhausted_progress": {"done": 3, "total": 5},
				"next.blocked": [
					{"id": "test-dep", "ticket": "test-dep-leaf", "reach": "held_fresh", "clearable": true},
					{"id": "test-ext", "ticket": "test-ext", "reach": "awaiting_outside", "clearable": false}
				],
				"next.held": [{"id": "test-held", "ticket": "test-held", "reach": "not_ready"}],
				"next.off_path": [],
				"next.hidden_by_filter": null,
				"next.unreachable": [],
				"next.stay_offered": true
			}`,
		},
		{
			name: "a blocker out of view keeps its place in walk order, and a withheld row is off the focus path",
			outcome: Exhausted{
				Epics:    []string{"test-epic"},
				Progress: claims.Progress{Done: 1, Total: 4},
				Blocked: []blockerReach{
					{depReach: readThrough("test-dep", "test-dep", reachNotReady)},
					{depReach: depReach{ID: "test-gone"}, Clearable: true},
					{depReach: readThrough("test-held", "test-held.1", reachHeldFresh), Clearable: true},
				},
				OffPath: []string{"test-off"},
			},
			gathered: 4,
			want: `{
				"next.outcome": "exhausted",
				"next.picked": null,
				"next.dependency_path": [],
				"next.exhausted_epics": ["test-epic"],
				"next.exhausted_progress": {"done": 1, "total": 4},
				"next.blocked": [
					{"id": "test-dep", "ticket": "test-dep", "reach": "not_ready", "clearable": false},
					{"id": "test-gone", "ticket": "", "reach": "out_of_view", "clearable": true},
					{"id": "test-held", "ticket": "test-held.1", "reach": "held_fresh", "clearable": true}
				],
				"next.held": [],
				"next.off_path": [{"id": "test-off", "ticket": "test-off", "reach": "off_focus_path"}],
				"next.hidden_by_filter": null,
				"next.unreachable": [],
				"next.stay_offered": true
			}`,
		},
		{
			name: "a blocker awaiting an outside event offers no stay",
			outcome: Exhausted{
				Epics:    []string{"test-epic"},
				Progress: claims.Progress{Done: 0, Total: 2},
				Blocked:  []blockerReach{{depReach: readThrough("test-ext", "test-ext", reachAwaitingOutside)}},
			},
			gathered: 2,
			want: `{
				"next.outcome": "exhausted",
				"next.picked": null,
				"next.dependency_path": [],
				"next.exhausted_epics": ["test-epic"],
				"next.exhausted_progress": {"done": 0, "total": 2},
				"next.blocked": [{"id": "test-ext", "ticket": "test-ext", "reach": "awaiting_outside", "clearable": false}],
				"next.held": [],
				"next.off_path": [],
				"next.hidden_by_filter": null,
				"next.unreachable": [],
				"next.stay_offered": false
			}`,
		},
		{
			name: "a pick served past the scope records how far the scope had come",
			outcome: ServedPastExhaustion{Row: rowFor("test-bug"), Exhaustion: Exhausted{
				Epics:    []string{"test-epic"},
				Progress: claims.Progress{Done: 8, Total: 9},
				Blocked:  []blockerReach{{depReach: readThrough("test-dep", "test-dep", reachHeldFresh), Clearable: true}},
			}},
			gathered: 3,
			want: `{
				"next.outcome": "served_past_exhaustion",
				"next.picked": "test-bug",
				"next.dependency_path": [],
				"next.exhausted_epics": ["test-epic"],
				"next.exhausted_progress": {"done": 8, "total": 9},
				"next.blocked": [{"id": "test-dep", "ticket": "test-dep", "reach": "held_fresh", "clearable": true}],
				"next.held": [],
				"next.off_path": [],
				"next.hidden_by_filter": null,
				"next.unreachable": [],
				"next.stay_offered": true
			}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := telemetry.Begin(telemetry.Event{})
			recordNextEvent(telemetry.With(context.Background(), recorder), tc.outcome, tc.gathered)
			ev := recorder.Finish(telemetry.End{})
			if got, want := writtenFacts(t, ev), decodeJSON(t, tc.want); !reflect.DeepEqual(got, want) {
				t.Errorf("facts =\n%v\nwant\n%v", got, want)
			}
			if n, ok := ev.Counts["next.rows_gathered"]; !ok || n != tc.gathered {
				t.Errorf("counts[next.rows_gathered] = %d, %v; want %d", n, ok, tc.gathered)
			}
		})
	}
}

// An empty pool under a focus label records every row it named: the ones the
// walk went past with their reach under next.unreachable, and the ones the
// focus withheld under next.off_path, the key an exhausted scope's withheld
// rows land on too.
func TestNextRecordsWhatAFocusedEmptyPoolNamed(t *testing.T) {
	recorder := telemetry.Begin(telemetry.Event{})
	outcome := NoWork{
		Unreachable: []rowReach{{ID: "test-gate", reading: reading{Row: rowFor("test-gate"), Kind: reachNotReady}}},
		OffPath:     []string{"test-off"},
	}
	recordNextEvent(telemetry.With(context.Background(), recorder), outcome, 2)
	want := decodeJSON(t, `{
		"unreachable": [{"id": "test-gate", "ticket": "test-gate", "reach": "not_ready"}],
		"off_path": [{"id": "test-off", "ticket": "test-off", "reach": "off_focus_path"}]
	}`)
	facts := writtenFacts(t, recorder.Finish(telemetry.End{}))
	if facts["next.outcome"] != "no_work" || !reflect.DeepEqual(facts["next.unreachable"], want["unreachable"]) || !reflect.DeepEqual(facts["next.off_path"], want["off_path"]) {
		t.Errorf("next.outcome, next.unreachable, next.off_path = %v, %v, %v; want no_work, %v, %v", facts["next.outcome"], facts["next.unreachable"], facts["next.off_path"], want["unreachable"], want["off_path"])
	}
}

// A dependency pick records the chain it was reached through, from the row of
// ours it gates down to the dependency it is work toward — the walk's depth is
// its length — and every other outcome records an empty one.
func TestNextRecordsTheChainADependencyPickWasReachedThrough(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome NextOutcome
		want    []any
	}{
		{
			name:    "a dependency of our row",
			outcome: ServedFromDependency{Row: rowFor("test-dep"), Gates: "test-ours", Blocker: "test-dep"},
			want:    []any{"test-ours", "test-dep"},
		},
		{
			name:    "a ready ticket under an epic behind a blocker that is not startable",
			outcome: ServedFromDependency{Row: rowFor("test-epic.1"), Gates: "test-ours", Via: []hop{{Prerequisite: "test-stuck", Through: "test-stuck"}}, Blocker: "test-epic"},
			want:    []any{"test-ours", "test-stuck", "test-epic"},
		},
		{
			name:    "a ready ticket behind a ticket under an epic that gates our row",
			outcome: ServedFromDependency{Row: rowFor("test-up"), Gates: "test-ours", Via: []hop{{Prerequisite: "test-gate", Through: "test-gate.1"}}, Blocker: "test-up"},
			want:    []any{"test-ours", "test-gate", "test-gate.1", "test-up"},
		},
		{
			name:    "a pick from the pool",
			outcome: ServedFromNewLane{Row: rowFor("test-fresh")},
			want:    []any{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := telemetry.Begin(telemetry.Event{})
			recordNextEvent(telemetry.With(context.Background(), recorder), tc.outcome, 1)
			if got := writtenFacts(t, recorder.Finish(telemetry.End{}))["next.dependency_path"]; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("next.dependency_path = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// A `lit next` that picks a ticket leaves the pick and how it was reached on
// its event.
func TestNextLeavesItsPickOnTheEvent(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Home")
	ticket := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--parent", epic, "--title", "Ready here", "--topic", "demo"))

	runCLIInDir(t, repo, "next")
	ev := lastEvent(t)
	if ev.Event != "lit next" || ev.Outcome != telemetry.OutcomeOK {
		t.Fatalf("event = %q %q, want lit next ok", ev.Event, ev.Outcome)
	}
	if ev.Facts["next.outcome"] != "served_from_new_lane" || ev.Facts["next.picked"] != ticket {
		t.Errorf("next.outcome, next.picked = %v, %v; want served_from_new_lane, %s", ev.Facts["next.outcome"], ev.Facts["next.picked"], ticket)
	}
}

// A `lit next` that finds nothing leaves every row it went past on its event,
// each with its reach — here a ticket waiting on an outside event — beside the
// exit code and reason the answer printed.
func TestNextLeavesTheRowsItWentPastOnTheEvent(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Home")
	waiting := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--parent", epic, "--title", "Upstream fix lands", "--topic", "demo", "--labels", ExternalLabel))

	_, err := runCLIInDirAllowError(t, repo, "next")
	if err == nil {
		t.Fatal("lit next served a ticket that waits on an outside event")
	}
	ev := lastEvent(t)
	if ev.Event != "lit next" || ev.ExitCode != ExitCode(err) || ev.Reason != string(commandErrorReason(err)) {
		t.Errorf("event = %q exit %d reason %q, want lit next exit %d reason %q", ev.Event, ev.ExitCode, ev.Reason, ExitCode(err), commandErrorReason(err))
	}
	want := decodeJSON(t, `{"unreachable": [{"id": "`+waiting+`", "ticket": "`+waiting+`", "reach": "awaiting_outside"}]}`)["unreachable"]
	if ev.Facts["next.outcome"] != "no_work" || !reflect.DeepEqual(ev.Facts["next.unreachable"], want) {
		t.Errorf("next.outcome, next.unreachable = %v, %v; want no_work, %v", ev.Facts["next.outcome"], ev.Facts["next.unreachable"], want)
	}
}

// A run the filter stopped records the ticket it hid — whether it served past
// it, served nothing, or found the scope exhausted with only the filter keeping
// the pool from it; a run the filter did not stop records none.
func TestNextRecordsTheTicketAFilterHidFromTheScope(t *testing.T) {
	for _, tc := range []struct {
		name        string
		outcome     NextOutcome
		wantOutcome string
		wantPicked  any
		wantHidden  any
	}{
		{
			name:        "served past the hidden ticket",
			outcome:     ServedPastNarrowing{Row: rowFor("test-bug"), Narrowing: Narrowed{Hidden: rowFor("test-ours")}},
			wantOutcome: "served_past_narrowing",
			wantPicked:  "test-bug",
			wantHidden:  "test-ours",
		},
		{
			name:        "nothing the filter selects is ready",
			outcome:     Narrowed{Hidden: rowFor("test-ours")},
			wantOutcome: "narrowed",
			wantPicked:  nil,
			wantHidden:  "test-ours",
		},
		{
			name:        "an exhausted scope whose pool the filter emptied",
			outcome:     Exhausted{Epics: []string{"test-epic"}, Narrowing: &Narrowed{Hidden: rowFor("test-theirs")}},
			wantOutcome: "exhausted",
			wantPicked:  nil,
			wantHidden:  "test-theirs",
		},
		{
			name:        "an exhausted scope with nothing ready anywhere",
			outcome:     Exhausted{Epics: []string{"test-epic"}},
			wantOutcome: "exhausted",
			wantPicked:  nil,
			wantHidden:  nil,
		},
		{
			name:        "an exhausted scope hid nothing",
			outcome:     ServedPastExhaustion{Row: rowFor("test-bug"), Exhaustion: Exhausted{Epics: []string{"test-epic"}}},
			wantOutcome: "served_past_exhaustion",
			wantPicked:  "test-bug",
			wantHidden:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := telemetry.Begin(telemetry.Event{})
			recordNextEvent(telemetry.With(context.Background(), recorder), tc.outcome, 1)
			facts := writtenFacts(t, recorder.Finish(telemetry.End{}))
			if facts["next.outcome"] != tc.wantOutcome || facts["next.picked"] != tc.wantPicked {
				t.Errorf("next.outcome, next.picked = %v, %v; want %v, %v", facts["next.outcome"], facts["next.picked"], tc.wantOutcome, tc.wantPicked)
			}
			if hidden, ok := facts["next.hidden_by_filter"]; !ok || hidden != tc.wantHidden {
				t.Errorf("next.hidden_by_filter = %v (present %v), want %v", hidden, ok, tc.wantHidden)
			}
		})
	}
}

// Driven through the real command, because the defect lived in what `lit next`
// handed routing: a held lane's only ready ticket is a task, and `--type bug`
// must name it as hidden rather than report the epic exhausted — on stderr, on
// the exit code and reason, and on the event. rows_gathered counts the rows the
// filter selected, which is none, not the queue routing read.
func TestNextUnderAFilterNamesTheTicketItHidFromTheHeldEpic(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Home")
	first := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--parent", epic, "--title", "First", "--topic", "demo"))
	second := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--parent", epic, "--title", "Second", "--topic", "demo"))
	// Finishing the first ticket leaves the epic's lane held by this checkout
	// with the second ready in it.
	runCLIInDir(t, repo, "start", first)
	runCLIInDir(t, repo, "done", first)

	_, err := runCLIInDirAllowError(t, repo, "next", "--type", "bug")
	if err == nil {
		t.Fatal("lit next --type bug served a ticket from a backlog holding no bug")
	}
	if strings.Contains(err.Error(), "no ready work in") {
		t.Fatalf("lit next --type bug = %q, want the epic not called exhausted — %s is ready in it", err.Error(), second)
	}
	if !strings.Contains(err.Error(), "`lit next` without --type, --status, --labels or --assignee hands you "+second+", which this run's filter leaves out") {
		t.Fatalf("lit next --type bug = %q, want it to name %s as the ticket the filter hid", err.Error(), second)
	}
	ev := lastEvent(t)
	if ev.ExitCode != ExitNoWork || ev.Reason != string(reasonScopeNarrowed) {
		t.Errorf("event exit %d reason %q, want %d %q", ev.ExitCode, ev.Reason, ExitNoWork, reasonScopeNarrowed)
	}
	if ev.Facts["next.outcome"] != "narrowed" || ev.Facts["next.hidden_by_filter"] != second {
		t.Errorf("next.outcome, next.hidden_by_filter = %v, %v; want narrowed, %s", ev.Facts["next.outcome"], ev.Facts["next.hidden_by_filter"], second)
	}
	if n, ok := ev.Counts["next.rows_gathered"]; !ok || n != 0 {
		t.Errorf("counts[next.rows_gathered] = %d, %v; want 0 — no row is a bug", n, ok)
	}
}

// A checkout holding no lane asks for bugs in a backlog of ready tasks. The
// filter emptied the pool, not the backlog, so the answer names the task
// `lit next` would hand over rather than reading as no ready work.
func TestNextUnderAFilterNamesThePoolTicketItHid(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "Home")
	task := extractTicketID(t, runCLIInDir(t, repo, "new", "--type", "task", "--parent", epic, "--title", "Ready", "--topic", "demo"))

	_, err := runCLIInDirAllowError(t, repo, "next", "--type", "bug")
	if err == nil {
		t.Fatal("lit next --type bug served a ticket from a backlog holding no bug")
	}
	if want := "`lit next` without --type, --status, --labels or --assignee hands you " + task + ", which this run's filter leaves out"; err.Error() != want {
		t.Fatalf("lit next --type bug = %q, want %q", err.Error(), want)
	}
	ev := lastEvent(t)
	if ev.ExitCode != ExitNoWork || ev.Reason != string(reasonScopeNarrowed) {
		t.Errorf("event exit %d reason %q, want %d %q", ev.ExitCode, ev.Reason, ExitNoWork, reasonScopeNarrowed)
	}
	if ev.Facts["next.outcome"] != "narrowed" || ev.Facts["next.hidden_by_filter"] != task {
		t.Errorf("next.outcome, next.hidden_by_filter = %v, %v; want narrowed, %s", ev.Facts["next.outcome"], ev.Facts["next.hidden_by_filter"], task)
	}
}
