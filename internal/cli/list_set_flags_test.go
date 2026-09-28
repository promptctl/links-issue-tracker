package cli

import (
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// setFlagSpelling is one way of asking for a filter set on the command line.
// Every spelling in a case must reach the same listing.
type setFlagSpelling struct {
	name string
	args []string
}

// setFlagCase is one set-valued filter: the spellings that must all select
// `want` and leave out `unwanted`.
type setFlagCase struct {
	flag      string
	spellings []setFlagSpelling
	want      []string
	unwanted  []string
}

// setFlagFixture seeds three issues that differ on type, id and labels at
// once, so one fixture serves every set flag: `bugA` is a bug labelled a,
// `taskAB` a task labelled a and b, `featB` a feature labelled b.
func setFlagFixture(t *testing.T) (h readyTestHarness, bugA, taskAB, featB string) {
	t.Helper()
	h = newReadyTestHarness(t)
	bug := h.createIssue(storage.CreateIssueInput{Title: "bug a", IssueType: model.TypeBug, Topic: "filtering", Labels: []string{"a"}})
	task := h.createIssue(storage.CreateIssueInput{Title: "task ab", IssueType: model.TypeTask, Topic: "filtering", Labels: []string{"a", "b"}})
	feat := h.createIssue(storage.CreateIssueInput{Title: "feature b", IssueType: model.TypeFeature, Topic: "filtering", Labels: []string{"b"}})
	return h, bug.ID, task.ID, feat.ID
}

// TestListSetFlagsWidenAcrossEverySpelling is the links-listing-uhc0
// acceptance. --type, --ids and --labels each feed a set the store already
// serves, and every spelling the CLI offers — comma list, repeated flag, query
// term, flag and term together — must reach the same set.
//
// The "flag, repeated" rows are the ones a regression fails: while these flags
// were single-value declarations, a second occurrence overwrote the first, so
// `--type bug --type task` answered with tasks only and said nothing about the
// bugs it dropped. [LAW:no-silent-failure]
//
// --labels is ALL-must-match, so its set narrows as it grows: naming a and b
// selects only the issue carrying both, and the overwrite regression shows up
// as the b-only issue leaking back in.
func TestListSetFlagsWidenAcrossEverySpelling(t *testing.T) {
	h, bugA, taskAB, featB := setFlagFixture(t)

	cases := []setFlagCase{
		{
			flag: "type",
			spellings: []setFlagSpelling{
				{"flag, comma-joined", []string{"--type", "bug,task"}},
				{"flag, repeated", []string{"--type", "bug", "--type", "task"}},
				{"query, comma-joined", []string{"--query", "type:bug,task"}},
				{"query, repeated terms", []string{"--query", "type:bug type:task"}},
				{"flag and query together", []string{"--type", "bug", "--query", "type:task"}},
			},
			want:     []string{bugA, taskAB},
			unwanted: []string{featB},
		},
		{
			flag: "ids",
			spellings: []setFlagSpelling{
				{"flag, comma-joined", []string{"--ids", bugA + "," + taskAB}},
				{"flag, repeated", []string{"--ids", bugA, "--ids", taskAB}},
				{"query, comma-joined", []string{"--query", "id:" + bugA + "," + taskAB}},
				{"query, repeated terms", []string{"--query", "id:" + bugA + " id:" + taskAB}},
				{"flag and query together", []string{"--ids", bugA, "--query", "id:" + taskAB}},
			},
			want:     []string{bugA, taskAB},
			unwanted: []string{featB},
		},
		{
			flag: "labels",
			spellings: []setFlagSpelling{
				{"flag, comma-joined", []string{"--labels", "a,b"}},
				{"flag, repeated", []string{"--labels", "a", "--labels", "b"}},
				{"query, comma-joined", []string{"--query", "label:a,b"}},
				{"query, repeated terms", []string{"--query", "label:a label:b"}},
				{"flag and query together", []string{"--labels", "a", "--query", "label:b"}},
			},
			want:     []string{taskAB},
			unwanted: []string{bugA, featB},
		},
	}
	for _, tc := range cases {
		for _, sp := range tc.spellings {
			t.Run(tc.flag+"/"+sp.name, func(t *testing.T) {
				got := runLs(t, h.ap, sp.args...)
				for _, id := range tc.want {
					if !strings.Contains(got, id) {
						t.Fatalf("lit ls %v omitted %q — an occurrence of the set was dropped; output:\n%s", sp.args, id, got)
					}
				}
				for _, id := range tc.unwanted {
					if strings.Contains(got, id) {
						t.Fatalf("lit ls %v leaked %q — the filter is not the set that was asked for; output:\n%s", sp.args, id, got)
					}
				}
			})
		}
	}
}

// TestListSetFlagsRejectABadMemberLoudly pins that widening bought no
// leniency. A typo anywhere in a type set, and an occurrence of any set flag
// that names nothing, must fail before a single row prints: a narrowed or
// unfiltered listing here has the exact shape of a real answer.
// [LAW:no-silent-failure]
//
// A blank member is a bad member in every set, free-text or sealed: `--ids
// "a,$MISSING"` must not quietly list only a.
func TestListSetFlagsRejectABadMemberLoudly(t *testing.T) {
	h, bugA, _, _ := setFlagFixture(t)

	for _, args := range [][]string{
		{"--type", "bug,bogus"},
		{"--type", "bogus,bug"},
		{"--type", "bug", "--type", "bogus"},
		{"--type", ""},
		{"--type", "bug,"},
		{"--query", "type:bug,bogus"},
		{"--query", "type:bug,"},
		{"--ids", ""},
		{"--ids", " , "},
		{"--ids", bugA, "--ids", ""},
		{"--ids", bugA + ","},
		{"--query", "id:"},
		{"--query", "id:" + bugA + ","},
		{"--labels", ""},
		{"--labels", "a", "--labels", ""},
		{"--labels", "a,"},
		{"--query", "label:"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out strings.Builder
			err := runListWithStore(h.ctx, &out, h.ap.Store, noReadyPolicy, args)
			if err == nil {
				t.Fatalf("lit ls %v returned no error; output:\n%s", args, out.String())
			}
			if out.Len() != 0 {
				t.Fatalf("lit ls %v printed %q before rejecting", args, out.String())
			}
		})
	}
}

// Every set-filter refusal, in either spelling, is a validation refusal
// (exit 3) naming the spelling that failed. Each repeats on every retry, so
// none may exit as a generic failure and draw retry-and-doctor advice, and a
// flag and its term must not fail differently for one malformed request.
func TestListSetFilterRefusalsAreValidationErrors(t *testing.T) {
	h, _, _, _ := setFlagFixture(t)
	for _, tc := range []struct {
		args  []string
		names string
	}{
		{[]string{"--status", "todo"}, "--status"},
		{[]string{"--type", "bogus"}, "--type"},
		{[]string{"--type="}, "--type"},
		{[]string{"--ids="}, "--ids"},
		{[]string{"--parent="}, "--parent"},
		{[]string{"--labels="}, "--labels"},
		{[]string{"--query", "status:todo"}, "todo"},
		{[]string{"--query", "type:bogus"}, "bogus"},
		{[]string{"--query", "id:"}, "id:"},
		{[]string{"--query", "parent:"}, "parent:"},
		{[]string{"--query", "label:"}, "label:"},
		// Refused while joining a term to the flags rather than while parsing
		// the term: the join is part of the same request.
		{[]string{"--has-comments=false", "--query", "has:comments"}, "has-comments"},
		{[]string{"--query", "updated>=2026-03-01T00:00:00Z updated<=2026-01-01T00:00:00Z"}, "updated"},
	} {
		err := runListWithStore(h.ctx, &strings.Builder{}, h.ap.Store, noReadyPolicy, tc.args)
		if got := ExitCode(err); got != ExitValidation {
			t.Fatalf("lit ls %v exit = %d (error %#v), want %d", tc.args, got, err, ExitValidation)
		}
		if !strings.Contains(err.Error(), tc.names) {
			t.Fatalf("lit ls %v error = %q, want it to name %q", tc.args, err, tc.names)
		}
	}
}
