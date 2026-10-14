package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The links-cli-hb6u acceptance: every set-valued flag outside `lit ls` widens
// its set when repeated. As single-value declarations, a second occurrence
// overwrote the first and the command acted on the narrowed value without a
// word. [LAW:no-silent-failure]
//
// Each case is built so the overwrite answers differently from the set, which
// is what makes reverting any one flag fail its own named subtest.

// lit backlog lists every row the filter admits, so the cases read like the
// lit ls ones: `want` must appear, `unwanted` must not.
func TestBacklogRepeatedSetFlagsWiden(t *testing.T) {
	h, bugA, taskAB, featB := setFlagFixture(t)
	for _, tc := range []setFlagCase{
		{flag: "type", spellings: []setFlagSpelling{{"repeated", []string{"--type", "bug", "--type", "task"}}, {"comma-joined", []string{"--type", "bug,task"}}}, want: []string{bugA, taskAB}, unwanted: []string{featB}},
		// ALL-must-match: the overwrite (b alone) leaks featB back in.
		{flag: "labels", spellings: []setFlagSpelling{{"repeated", []string{"--labels", "a", "--labels", "b"}}, {"comma-joined", []string{"--labels", "a,b"}}}, want: []string{taskAB}, unwanted: []string{bugA, featB}},
		{flag: "assignee", spellings: []setFlagSpelling{{"repeated", []string{"--assignee", "ann", "--assignee", "bob"}}, {"comma-joined", []string{"--assignee", "ann,bob"}}}, want: []string{bugA, taskAB}, unwanted: []string{featB}},
	} {
		for _, sp := range tc.spellings {
			t.Run(tc.flag+"/"+sp.name, func(t *testing.T) {
				got := h.runWorkableText(sp.args...)
				for _, id := range tc.want {
					if !strings.Contains(got, id) {
						t.Fatalf("lit backlog %v omitted %q — an occurrence of the set was dropped; output:\n%s", sp.args, id, got)
					}
				}
				for _, id := range tc.unwanted {
					if strings.Contains(got, id) {
						t.Fatalf("lit backlog %v leaked %q — the filter is not the set that was asked for; output:\n%s", sp.args, id, got)
					}
				}
			})
		}
	}
}

// lit next serves one row, the highest-ranked the filter admits: bugA, then
// taskAB, then featB. Each spelling puts the flag whose overwrite would serve a
// different row LAST, so the set and the overwrite serve different tickets.
func TestNextRepeatedSetFlagsWiden(t *testing.T) {
	h, bugA, taskAB, _ := setFlagFixture(t)
	for _, tc := range []struct {
		flag string
		args []string
		want string
	}{
		// Overwrite: task only, serves taskAB. Set: bug or task, serves bugA.
		{"type", []string{"--type", "bug", "--type", "task"}, bugA},
		// Overwrite: a only, serves bugA. Set: a and b, serves taskAB.
		{"labels", []string{"--labels", "b", "--labels", "a"}, taskAB},
		// Overwrite: bob only, serves taskAB. Set: ann or bob, serves bugA.
		{"assignee", []string{"--assignee", "ann", "--assignee", "bob"}, bugA},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			if got := h.runNextRowArgs(tc.args...); got.ID != tc.want {
				t.Fatalf("lit next %v served %q, want %q — a repeated flag kept only its last occurrence", tc.args, got.ID, tc.want)
			}
		})
	}
}

// A blank member is a bad member in every set, on backlog and next as on ls:
// a validation refusal before anything prints, never a narrower answer.
func TestWorkableSetFlagsRejectABadMemberLoudly(t *testing.T) {
	h, _, _, _ := setFlagFixture(t)
	for _, args := range [][]string{
		{"--type", "bug", "--type", "bogus"},
		{"--type", ""},
		{"--labels", "a", "--labels", ""},
		{"--labels", "a,"},
		{"--assignee", "ann", "--assignee", ""},
		{"--assignee", ""},
	} {
		for name, run := range map[string]func(*bytes.Buffer) error{
			"backlog": func(out *bytes.Buffer) error { return runWorkable(h.ctx, out, h.ap, args, backlogView) },
			"next":    func(out *bytes.Buffer) error { return runNext(h.ctx, out, h.ap, args) },
		} {
			t.Run(name+" "+strings.Join(args, " "), func(t *testing.T) {
				var out bytes.Buffer
				err := run(&out)
				if got := ExitCode(err); got != ExitValidation {
					t.Fatalf("lit %s %v exit = %d (error %v), want %d", name, args, got, err, ExitValidation)
				}
				if out.Len() != 0 {
					t.Fatalf("lit %s %v printed %q before rejecting", name, args, out.String())
				}
			})
		}
	}
}

// lit orphaned narrows by assignee through the same set: repeating the flag
// lists both holders' stale work.
func TestOrphanedRepeatedAssigneeWidens(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Title: "orphans", IssueType: model.TypeEpic, Topic: "orphans"})
	var stale []string
	for _, holder := range []string{"ann", "bob", "cy"} {
		issue := h.createIssue(storage.CreateIssueInput{Title: holder + "'s work", IssueType: model.TypeTask, Topic: "orphans", ParentID: epic.ID, Lane: holder})
		if _, err := h.ap.Store.Apply(h.ctx, issue.ID, storage.Change{Action: model.Start{Assignee: holder}, Actor: holder}); err != nil {
			t.Fatalf("start %s: %v", issue.ID, err)
		}
		h.backdateUpdatedAt(issue.ID, 7*time.Hour)
		stale = append(stale, issue.ID)
	}
	got := strings.Join(runOrphanedIDs(t, h, "--assignee", "ann", "--assignee", "bob"), " ")
	if !strings.Contains(got, stale[0]) || !strings.Contains(got, stale[1]) || strings.Contains(got, stale[2]) {
		t.Fatalf("lit orphaned --assignee ann --assignee bob = [%s], want exactly ann's %s and bob's %s", got, stale[0], stale[1])
	}
}

// The bulk commands WRITE, so the overwrite was the worst form of this bug:
// `lit bulk close --ids A --ids B` closed B and skipped A. Each bulk command is
// handed two ids in two occurrences and must act on both.
func TestBulkRepeatedIDsActOnEveryID(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(h readyTestHarness, out *bytes.Buffer, idArgs []string) error
		// listing is a lit ls in which an issue's presence says whether the
		// command acted on it: present when actedShows, absent otherwise.
		listing    []string
		actedShows bool
	}{
		{"label", func(h readyTestHarness, out *bytes.Buffer, idArgs []string) error {
			return runBulkLabel(h.ctx, out, h.ap, append(append([]string{"add"}, idArgs...), "--label", "swept"))
		}, []string{"--labels", "swept"}, true},
		{"close", func(h readyTestHarness, out *bytes.Buffer, idArgs []string) error {
			return runBulkClose(h.ctx, out, h.ap, append(idArgs, "--resolution", "obsolete"))
		}, []string{"--status", "closed"}, true},
		{"archive", func(h readyTestHarness, out *bytes.Buffer, idArgs []string) error {
			return runLeaf(bulkTransitionLeaf(model.Archive{})(), h.ctx, out, h.ap, idArgs)
		}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, bugA, taskAB, featB := setFlagFixture(t)
			var out bytes.Buffer
			if err := tc.run(h, &out, []string{"--ids", bugA, "--ids", featB}); err != nil {
				t.Fatalf("lit bulk %s --ids %s --ids %s: %v", tc.name, bugA, featB, err)
			}
			got := runLs(t, h.ap, tc.listing...)
			for id, given := range map[string]bool{bugA: true, featB: true, taskAB: false} {
				if acted := strings.Contains(got, id) == tc.actedShows; acted != given {
					t.Fatalf("lit bulk %s --ids %s --ids %s: acted on %q = %v, want %v; lit ls %v:\n%s", tc.name, bugA, featB, id, acted, given, tc.listing, got)
				}
			}
		})
	}
}

// A blank --ids occurrence refuses the whole command before any id is acted
// on: `--ids A --ids "$UNSET"` must not quietly act on A alone.
func TestBulkBlankIDRefusesBeforeActing(t *testing.T) {
	h, bugA, _, _ := setFlagFixture(t)
	var out bytes.Buffer
	err := runBulkClose(h.ctx, &out, h.ap, []string{"--ids", bugA, "--ids", "", "--resolution", "obsolete"})
	if got := ExitCode(err); got != ExitValidation {
		t.Fatalf("lit bulk close --ids %s --ids \"\" exit = %d (error %v), want %d", bugA, got, err, ExitValidation)
	}
	if closed := runLs(t, h.ap, "--status", "closed"); strings.Contains(closed, bugA) {
		t.Fatalf("lit bulk close closed %q before refusing the blank id:\n%s", bugA, closed)
	}
}

// Every single-value flag refuses a second occurrence, whatever its kind.
// pflag's own scalars keep the last value and drop the earlier ones without a
// word. Each case also parses its one-occurrence spelling, so the refusal is
// pinned to the repeat and not to the flag.
func TestSingleValueFlagRefusesARepeat(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		declare func(fs *cobraFlagSet)
		once    []string
		twice   []string
	}{
		{"String", func(fs *cobraFlagSet) { fs.String("x", "", "") }, []string{"--x", "a"}, []string{"--x", "a", "--x", "b"}},
		{"Bool", func(fs *cobraFlagSet) { fs.Bool("x", false, "") }, []string{"--x"}, []string{"--x", "--x=false"}},
		{"Int", func(fs *cobraFlagSet) { fs.Int("x", 0, "") }, []string{"--x", "1"}, []string{"--x", "1", "--x", "2"}},
		{"StringOptional", func(fs *cobraFlagSet) { fs.StringOptional("x", "all", "", "") }, []string{"--x=a"}, []string{"--x=a", "--x"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			once := newCobraFlagSet("probe")
			tc.declare(once)
			if err := parseFlagSet(once, tc.once, io.Discard); err != nil {
				t.Fatalf("%s flag given once %v: %v, want it parsed", tc.kind, tc.once, err)
			}
			twice := newCobraFlagSet("probe")
			tc.declare(twice)
			err := parseFlagSet(twice, tc.twice, io.Discard)
			if err == nil || ExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "given more than once") {
				t.Fatalf("%s flag given twice %v: error %v, want a usage refusal saying it was given more than once", tc.kind, tc.twice, err)
			}
		})
	}
}

// The write the single-value refusal exists for: `lit new --labels a --labels
// b` stored b alone. It is refused before anything is created.
func TestNewRefusesARepeatedLabelsFlagBeforeWriting(t *testing.T) {
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Title: "home", IssueType: model.TypeEpic, Topic: "repeat"})
	var out bytes.Buffer
	err := runNew(h.ctx, &out, h.ap, []string{"--title", "repeat probe", "--topic", "repeat", "--type", "task", "--parent", epic.ID, "--labels", "a", "--labels", "b"})
	if got := ExitCode(err); got != ExitUsage {
		t.Fatalf("lit new --labels a --labels b exit = %d (error %v), want %d", got, err, ExitUsage)
	}
	if listed := runLs(t, h.ap, "--search", "repeat probe"); strings.TrimSpace(listed) != "" {
		t.Fatalf("lit new created an issue before refusing the repeated flag:\n%s", listed)
	}
}
