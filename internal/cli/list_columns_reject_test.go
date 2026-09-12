package cli

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// These tests are the accept/reject table for `--columns`, written out case by
// case. The bug they close is a silent one: an unknown name used to be dropped
// and the remaining columns printed under exit 0, so a typo read back as a
// successful answer to a different question. Nothing but an explicit reject-set
// catches that, because every wrong answer it produced was well-formed.

// listColumnsOutput runs `lit ls --columns expr` against a store holding one
// issue and returns stdout and the error, so each case can assert on both. A
// rejection that still printed rows would be a partial answer, which is the
// failure mode this whole boundary exists to prevent.
func listColumnsOutput(t *testing.T, expr string) (string, error) {
	t.Helper()
	ctx := context.Background()
	ap := newTestCLIApp(t)
	if _, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "col", Title: "T", Topic: "col", IssueType: "task", Priority: 0}); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	var out bytes.Buffer
	err := runListWithStore(ctx, &out, ap.Store, noReadyPolicy, []string{"--columns", expr})
	return out.String(), err
}

// TestColumnsRejectsUnknownName is the reject half of the table. The four
// vocabulary rows carry the most weight: status, description, prompt and lane
// are all real `lit show --field` names, so they are exactly what someone types
// after learning the field vocabulary — and `status` is the one from the bug
// report, where the column is spelled `state`.
func TestColumnsRejectsUnknownName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expr    string
		unknown string
	}{
		{name: "unknown word", expr: "bogus", unknown: "bogus"},
		{name: "field vocabulary: status is spelled state as a column", expr: "status", unknown: "status"},
		{name: "field vocabulary: description is multi-line", expr: "description", unknown: "description"},
		{name: "field vocabulary: prompt is multi-line", expr: "prompt", unknown: "prompt"},
		{name: "field vocabulary: lane needs the epic to be canonical", expr: "lane", unknown: "lane"},
		{name: "one bad name among good ones", expr: "id,bogus,title", unknown: "bogus"},
		{name: "bad name normalized before reporting", expr: " BOGUS ", unknown: "bogus"},
		// The near-misses. Membership is exact, and these are the rows that say
		// so: each one is a substring or superstring of a real column, so a
		// matcher that loosened to HasPrefix/Contains — the usual way an
		// accept-set decays — would accept them while every row above still
		// passed.
		{name: "prefix of a valid name", expr: "stat", unknown: "stat"},
		{name: "valid name with a suffix", expr: "state1", unknown: "state1"},
		{name: "valid name with a prefix", expr: "xstate", unknown: "xstate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := listColumnsOutput(t, tc.expr)
			if err == nil {
				t.Fatalf("--columns %q: want error, got nil (output:\n%s)", tc.expr, out)
			}
			if got := ExitCode(err); got != ExitUsage {
				t.Errorf("--columns %q exit code = %d, want %d (ExitUsage)", tc.expr, got, ExitUsage)
			}
			// Matched against the QUOTED offender, not the bare word. The
			// message always carries the full valid-columns list, so a bare
			// substring check is satisfied by an unrelated part of it — "stat"
			// is inside the "state" this very message advertises, and that row
			// passed no matter what the code echoed as the offender. Requiring
			// the quotes puts the match on the one span only the offender fills.
			if quoted := fmt.Sprintf("%q", tc.unknown); !strings.Contains(err.Error(), quoted) {
				t.Errorf("--columns %q error %q does not name the offending column as %s", tc.expr, err, quoted)
			}
			// The rejection has to hand back the whole accepted vocabulary;
			// a bare "unknown column" leaves the caller guessing the spelling
			// they got wrong, which is how `state` stayed undiscovered.
			for _, valid := range sortedColumnNames() {
				if !strings.Contains(err.Error(), valid) {
					t.Errorf("--columns %q error %q omits valid column %q", tc.expr, err, valid)
				}
			}
			// Validated before anything prints: a partial table is itself a
			// silent drop wearing an error message.
			if out != "" {
				t.Errorf("--columns %q printed output before rejecting:\n%s", tc.expr, out)
			}
		})
	}
}

// TestColumnsAcceptsEveryDeclaredName is the accept half: every name the
// rejection message advertises has to actually work, or the error is lying
// about the vocabulary. This is what keeps the accept-set and the renderer from
// drifting apart again — the drift that left `rank` printable as a field and
// unprintable as a column.
func TestColumnsAcceptsEveryDeclaredName(t *testing.T) {
	for _, name := range sortedColumnNames() {
		t.Run(name, func(t *testing.T) {
			out, err := listColumnsOutput(t, name)
			if err != nil {
				t.Fatalf("--columns %s: advertised as valid but rejected: %v", name, err)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("--columns %s: accepted but rendered no cell", name)
			}
		})
	}
}

// TestColumnsAcceptsCaseAndSpacing pins the normalization the old parser did, so
// tightening the boundary did not also start rejecting input that always worked.
func TestColumnsAcceptsCaseAndSpacing(t *testing.T) {
	for _, expr := range []string{"ID,TITLE", " id , title ", "Id,Title"} {
		if _, err := listColumnsOutput(t, expr); err != nil {
			t.Errorf("--columns %q: want accepted, got %v", expr, err)
		}
	}
}

// TestColumnsBlankExpressionKeepsDefaultProjection: an empty expression is the
// caller asking for nothing in particular, which is the default projection and
// not an error. splitCSV also drops empty elements, so a stray comma lands here
// too rather than in the reject set.
//
// The default is spelled out here as literal text rather than compared against
// defaultColumnSet(), which would only compare the code with itself and pass no
// matter which columns the default named.
func TestColumnsBlankExpressionKeepsDefaultProjection(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "dflt", Title: "T", Topic: "dflt", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	want := []string{issue.ID, "open", "dflt", "T"}
	for _, args := range [][]string{
		{},
		{"--columns", ""},
		{"--columns", " "},
		{"--columns", ","},
	} {
		var out bytes.Buffer
		if err := runListWithStore(ctx, &out, ap.Store, noReadyPolicy, args); err != nil {
			t.Fatalf("lit ls %v: want default projection, got %v", args, err)
		}
		got := fieldsOf(lineForID(t, out.String(), issue.ID))
		if !slices.Equal(got, want) {
			t.Errorf("lit ls %v rendered %v, want the default projection %v", args, got, want)
		}
	}
}

// TestColumnsRankRendersTheIssueRank settles the question the ticket left open:
// rank is a printable column, not an error. It renders the issue's own rank, so
// the listing can show the key it is ordered by.
func TestColumnsRankRendersTheIssueRank(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, err := ap.Store.CreateIssue(ctx, storage.CreateIssueInput{Prefix: "rank", Title: "T", Topic: "rank", IssueType: "task", Priority: 0})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	var out bytes.Buffer
	if err := runListWithStore(ctx, &out, ap.Store, noReadyPolicy, []string{"--columns", "id,rank"}); err != nil {
		t.Fatalf("--columns id,rank: %v", err)
	}
	fields := fieldsOf(lineForID(t, out.String(), issue.ID))
	if len(fields) != 2 {
		t.Fatalf("--columns id,rank: want 2 fields, got %d: %v", len(fields), fields)
	}
	if fields[1] != issue.Rank {
		t.Errorf("rank column = %q, want the issue's rank %q", fields[1], issue.Rank)
	}
}

// TestColumnsHelpEnumeratesValidNames: the vocabulary has to be discoverable
// without first provoking a rejection, on every surface that offers the flag.
func TestColumnsHelpEnumeratesValidNames(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)
	var out bytes.Buffer
	// --help is answered by the flag parser, which reports it via a sentinel
	// error after printing; the output is what this asserts on.
	_ = runListWithStore(ctx, &out, ap.Store, noReadyPolicy, []string{"--help"})
	for _, name := range sortedColumnNames() {
		if !strings.Contains(out.String(), name) {
			t.Errorf("`lit ls --help` does not enumerate column %q:\n%s", name, out.String())
		}
	}
}

// TestColumnRegistryIsWellFormed pins what every derived surface — the
// accept-set, the help text, the rejection message, the relation-load decision
// — assumes about the table it reads. A duplicate name is the defect none of
// the tests above can see: columnsByName would silently keep the last entry
// while the advertised vocabulary listed the name twice and both lookups
// "worked".
func TestColumnRegistryIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for i, spec := range columnRegistry {
		if spec.name == "" {
			t.Errorf("columnRegistry[%d] has an empty name", i)
		}
		if spec.render == nil {
			t.Errorf("column %q declares no renderer, so selecting it would render a short row", spec.name)
		}
		if seen[spec.name] {
			t.Errorf("column %q is declared twice; the name index keeps only the last", spec.name)
		}
		seen[spec.name] = true
	}
	if len(sortedColumnNames()) != len(columnRegistry) {
		t.Errorf("advertised vocabulary has %d names for %d registry entries",
			len(sortedColumnNames()), len(columnRegistry))
	}
}

// TestMustColumnsPanicsOnUnknownName is the loud-failure arm for the
// projections this package writes itself. Those names are source constants
// rather than user input, so the answer to a miss is a crash naming the column
// — never the short row that silently dropping it would print.
func TestMustColumnsPanicsOnUnknownName(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("mustColumns(bogus): want panic, got none")
		}
		if !strings.Contains(fmt.Sprint(recovered), "bogus") {
			t.Errorf("panic %v does not name the offending column", recovered)
		}
	}()
	mustColumns("id", "bogus")
}
