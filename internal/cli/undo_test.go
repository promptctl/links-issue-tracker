package cli

import (
	"strings"
	"testing"
)

// listedWrite is the id of the write `lit undo list` shows on its first line.
func listedWrite(t *testing.T, listing string) string {
	t.Helper()
	first, _, _ := strings.Cut(listing, "\n")
	id, _, ok := strings.Cut(first, "  ")
	if !ok {
		t.Fatalf("lit undo list first line %q has no write id", first)
	}
	return id
}

// The ticket's case through the command line: a reparent is listed with the
// parent it moved, reverting it puts the old parent back, the command says
// what it put back and which writes undo the undo, and the command's event
// carries what it decided.
func TestUndoRevertPutsAReparentBackThroughTheCommandLine(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	from := newEpicInDir(t, repo, "from")
	to := newEpicInDir(t, repo, "to")
	task := extractTicketID(t, runCLIInDir(t, repo, "new", "--parent", from, "--title", "task", "--topic", "demo"))
	runCLIInDir(t, repo, "parent", "set", "--child", task, "--parent", to)

	listing := runCLIInDir(t, repo, "undo", "list", "--last", "1")
	if want := task + " parent: " + from + " → " + to; !strings.Contains(listing, want) {
		t.Fatalf("lit undo list =\n%s\nwant the move %q", listing, want)
	}
	reparent := listedWrite(t, listing)
	if ev := lastEvent(t); ev.Facts["undo.listed"] != float64(1) || ev.Facts["undo.revertable"] != float64(1) {
		t.Errorf("list event facts = %v, want one listed and one revertable", ev.Facts)
	}

	out := runCLIInDir(t, repo, "undo", "revert", "--writes", reparent)
	for _, want := range []string{"reverted 1 writes", "put back 1", task + " parent: " + to + " → " + from, "landed as"} {
		if !strings.Contains(out, want) {
			t.Errorf("lit undo revert =\n%s\nwant %q", out, want)
		}
	}
	ev := lastEvent(t)
	if ev.Facts["undo.slots"] != float64(1) || ev.Facts["undo.conflicts"] != float64(0) || ev.Facts["undo.unrevertable"] != float64(0) {
		t.Errorf("revert event facts = %v, want one slot, no conflicts, nothing unrevertable", ev.Facts)
	}
	if landed, ok := ev.Facts["undo.landed"].([]any); !ok || len(landed) == 0 {
		t.Errorf("revert event undo.landed = %v, want the commits it landed", ev.Facts["undo.landed"])
	}
	if show := runCLIInDir(t, repo, "show", task); !strings.Contains(show, from) {
		t.Errorf("lit show %s after the undo =\n%s\nwant its parent %s back", task, show, from)
	}
}

// landedWrites is the ids `lit undo revert` lists under "landed as".
func landedWrites(t *testing.T, out string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(out, "landed as")
	if !ok {
		t.Fatalf("lit undo revert =\n%s\nhas no landed writes", out)
	}
	var ids []string
	for _, line := range strings.Split(rest, "\n")[1:] {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// A refused undo exits on its reason and writes nothing. Reverting a write a
// later write moved again is a conflict; a write named beside its own undo
// nets to nothing, so the state asked for already holds; a lifecycle write is
// refused as the command issued.
func TestUndoRevertAnswersOnItsReasons(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	epic := newEpicInDir(t, repo, "epic")
	other := newEpicInDir(t, repo, "other")
	task := extractTicketID(t, runCLIInDir(t, repo, "new", "--parent", epic, "--title", "task", "--topic", "demo"))
	runCLIInDir(t, repo, "parent", "set", "--child", task, "--parent", other)
	reparent := listedWrite(t, runCLIInDir(t, repo, "undo", "list", "--last", "1"))
	undo := landedWrites(t, runCLIInDir(t, repo, "undo", "revert", "--writes", reparent))

	cases := []struct {
		name, reason string
		writes       func() string
	}{
		{"a write a later write moved again", "undo_conflict", func() string { return reparent }},
		{"a write beside its own undo", "state_already_holds", func() string { return strings.Join(append([]string{reparent}, undo...), ",") }},
		{"a lifecycle write", "validation_refused", func() string {
			runCLIInDir(t, repo, "start", task, "--by", "ada")
			return listedWrite(t, runCLIInDir(t, repo, "undo", "list", "--last", "1"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writes := tc.writes()
			before := listedWrite(t, runCLIInDir(t, repo, "undo", "list", "--last", "1"))
			_, err := runCLIInDirAllowError(t, repo, "undo", "revert", "--writes", writes)
			if got := commandErrorReason(err); got.String() != tc.reason {
				t.Fatalf("lit undo revert %s error = %v (reason %s), want reason %s", writes, err, got, tc.reason)
			}
			if ev := lastEvent(t); ev.Reason != tc.reason {
				t.Errorf("event reason = %q, want %q", ev.Reason, tc.reason)
			}
			if after := listedWrite(t, runCLIInDir(t, repo, "undo", "list", "--last", "1")); after != before {
				t.Errorf("a refused undo wrote %s", after)
			}
		})
	}
}

// A --writes slot with no write id is refused in the flag's own terms.
func TestUndoRevertRefusesAnEmptyWriteSlotByItsFlag(t *testing.T) {
	isolateEvents(t)
	repo := newInitTestRepo(t)
	_, err := runCLIInDirAllowError(t, repo, "undo", "revert", "--writes", "abc,,def")
	if err == nil || !strings.Contains(err.Error(), "--writes needs a write id") {
		t.Fatalf("lit undo revert --writes abc,,def error = %v, want the --writes refusal", err)
	}
}
