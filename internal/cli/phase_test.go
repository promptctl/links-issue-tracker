package cli

import (
	"reflect"
	"strings"
	"testing"
)

// phaseFacts is what the last invocation's event says its phase move did.
func phaseFacts(t *testing.T) map[string]any {
	t.Helper()
	facts := map[string]any{}
	for key, value := range writtenFacts(t, lastEvent(t)) {
		if strings.HasPrefix(key, "phase.") {
			facts[key] = value
		}
	}
	return facts
}

// lit phase moves an in-progress ticket's phase: the event names the phase
// left, the phase entered and whether anything changed; lit show and
// `--field phase` read the phase back; a move to the phase the ticket is in
// records nothing; a ticket that is not in progress refuses the move; and the
// statusline file keeps naming the ticket being worked on.
func TestPhaseMovesAnInProgressTicket(t *testing.T) {
	isolateEvents(t)
	dir, first, second := statuslineCheckout(t)
	runCLIInDir(t, dir, "start", first, "--by", "agent")

	for _, step := range []struct {
		to      string
		from    string
		changed bool
		notice  string
	}{
		{"impl", "plan", true, "phase: plan → impl\n"},
		{"impl", "impl", false, "phase: impl (unchanged)\n"},
		{"review", "impl", true, "phase: impl → review\n"},
		{"impl", "review", true, "phase: review → impl\n"},
	} {
		if out := runCLIInDir(t, dir, "phase", first, "--to", step.to); !strings.HasPrefix(out, step.notice+first+" ") {
			t.Fatalf("lit phase --to %s printed %q, want the line %q before the ticket's summary", step.to, out, step.notice)
		}
		want := map[string]any{"phase.from": step.from, "phase.to": step.to, "phase.changed": step.changed}
		if got := phaseFacts(t); !reflect.DeepEqual(got, want) {
			t.Fatalf("lit phase --to %s event facts = %v, want %v", step.to, got, want)
		}
		if got := runCLIInDir(t, dir, "show", first, "--field", "phase"); got != step.to+"\n" {
			t.Fatalf("show --field phase after --to %s = %q, want %q", step.to, got, step.to+"\n")
		}
	}
	if show := runCLIInDir(t, dir, "show", first); !strings.Contains(show, "\nstatus: in_progress\nphase: impl\n") {
		t.Errorf("lit show does not print the phase under the status:\n%s", show)
	}
	history := runCLIInDir(t, dir, "history", first)
	if got := strings.Count(history, "phase: plan → impl"); got != 1 {
		t.Errorf("history records plan → impl %d times, want once (the repeated move records nothing):\n%s", got, history)
	}
	if got := workingOnID(readStatusline(t, dir)); got != first {
		t.Errorf("statusline working_on = %q after phase moves, want %q", got, first)
	}

	out, err := runCLIInDirAllowError(t, dir, "phase", second, "--to", "impl")
	if err == nil || !strings.Contains(out+err.Error(), "only an in-progress ticket has a phase") {
		t.Errorf("lit phase on an open ticket = %q, %v; want the in-progress refusal", out, err)
	}
	if got := runCLIInDir(t, dir, "show", second, "--field", "phase"); got != "\n" {
		t.Errorf("show --field phase on an open ticket = %q, want an empty value", got)
	}
}

// A mis-written `lit phase` is a usage error whatever the id names, and every
// usage line it prints is the command's one line, naming the required --to.
func TestPhaseUsageNamesTheRequiredPhase(t *testing.T) {
	dir, _, _ := statuslineCheckout(t)
	const usage = "usage: lit phase <id> --to <plan|impl|review|merge> [--reason <text>]"
	for _, args := range [][]string{
		{"phase"},
		{"phase", "no-such-id"},
		{"phase", "no-such-id", "--to", "shipping"},
	} {
		out, err := runCLIInDirAllowError(t, dir, args...)
		if got := ExitCode(err); got != ExitUsage {
			t.Errorf("lit %v exit = %d (%v), want %d, a usage error", args, got, err, ExitUsage)
		}
		if !strings.Contains(out+err.Error(), usage) {
			t.Errorf("lit %v = %q, %v; want the usage line %q", args, out, err, usage)
		}
	}
}
