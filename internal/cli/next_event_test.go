package cli

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/annotation"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// Every reach kind has its own name on the event; a kind left unnamed would
// record a blocker with no reach at all.
func TestEveryReachKindHasAnEventName(t *testing.T) {
	seen := map[string]reachKind{}
	for kind := reachKind(0); kind < reachKindCount; kind++ {
		name := reachKindNames[kind]
		if name == "" {
			t.Errorf("reach kind %d has no event name", kind)
		}
		if other, dup := seen[name]; dup {
			t.Errorf("reach kinds %d and %d share the event name %q", other, kind, name)
		}
		seen[name] = kind
	}
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

// An exhausted scope's event names every blocker and held row with what this
// checkout may do about each, and whether the route that stays in the scope
// was offered — it is, exactly when some blocker is one a ticket filed here
// could clear.
func TestNextRecordsWhatAnExhaustedScopeNamed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outcome  Exhausted
		gathered int
		want     string
	}{
		{
			name: "a blocker another checkout holds can be cleared from here",
			outcome: Exhausted{
				Epics: []string{"test-epic"},
				Blocked: []rowReach{
					{ID: "test-dep", Row: rowFor("test-dep-leaf"), Kind: reachHeldFresh},
					{ID: "test-ext", Row: rowFor("test-ext"), Kind: reachAwaitingOutside},
				},
				Held: []rowReach{{ID: "test-held", Row: rowFor("test-held"), Kind: reachNotReady}},
			},
			gathered: 5,
			want: `{
				"next.outcome": "exhausted",
				"next.picked": null,
				"next.dependency_path": [],
				"next.exhausted_epics": ["test-epic"],
				"next.blocked": [
					{"id": "test-dep", "ticket": "test-dep-leaf", "reach": "held_fresh"},
					{"id": "test-ext", "ticket": "test-ext", "reach": "awaiting_outside"}
				],
				"next.held": [{"id": "test-held", "ticket": "test-held", "reach": "not_ready"}],
				"next.off_path": [],
				"next.hidden_by_filter": null,
				"next.unreachable": [],
				"next.stay_offered": true
			}`,
		},
		{
			name: "a blocker awaiting an outside event offers no stay",
			outcome: Exhausted{
				Epics:   []string{"test-epic"},
				Blocked: []rowReach{{ID: "test-ext", Row: rowFor("test-ext"), Kind: reachAwaitingOutside}},
			},
			gathered: 2,
			want: `{
				"next.outcome": "exhausted",
				"next.picked": null,
				"next.dependency_path": [],
				"next.exhausted_epics": ["test-epic"],
				"next.blocked": [{"id": "test-ext", "ticket": "test-ext", "reach": "awaiting_outside"}],
				"next.held": [],
				"next.off_path": [],
				"next.hidden_by_filter": null,
				"next.unreachable": [],
				"next.stay_offered": false
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

// A scope stopped by the filter rather than by its work records the ticket the
// filter hid, served past or not; a scope that really is exhausted records none.
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
	if !strings.Contains(err.Error(), second+" is the next ticket on your path, and this run's filter leaves it out") {
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
