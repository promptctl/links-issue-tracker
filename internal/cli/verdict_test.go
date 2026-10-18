package cli

import (
	"reflect"
	"strings"
	"testing"
)

// verdictFacts is what the last invocation's event says about the verdict it
// recorded.
func verdictFacts(t *testing.T) map[string]any {
	t.Helper()
	facts := map[string]any{}
	for key, value := range writtenFacts(t, lastEvent(t)) {
		if strings.HasPrefix(key, "verdict.") {
			facts[key] = value
		}
	}
	return facts
}

// A plan is written with lit update --plan, and each edit is a version. lit
// verdict records a review of the version the reviewer names. lit show prints
// the current plan under its version and only the reviews of that version; lit
// history places each review in the trail by the version it judged, and prints
// no plan text, current or superseded.
func TestAVerdictJudgesOnePlanVersion(t *testing.T) {
	isolateEvents(t)
	dir, id, _ := statuslineCheckout(t)
	const first, second = "Add a retry around the push", "Drop the retry; the lease already bounds it"
	runCLIInDir(t, dir, "update", id, "--plan", first, "--by", "author")
	runCLIInDir(t, dir, "update", id, "--plan", second, "--by", "author")

	if show := runCLIInDir(t, dir, "show", id); !strings.Contains(show, "\nplan (v2):\n"+second+"\nreviews of plan v2: none — lit verdict records one\n") {
		t.Fatalf("lit show before any review does not print plan v2 as unreviewed:\n%s", show)
	}

	out := runCLIInDir(t, dir, "verdict", id, "fail", "--version", "1", "--law", "one-source-of-truth", "--law", "carrying-cost", "--by", "reviewer")
	if want := ": fail on plan v1 — laws: carrying-cost, one-source-of-truth\n"; !strings.HasPrefix(out, id+" rvw-") || !strings.HasSuffix(out, want) {
		t.Errorf("lit verdict printed %q, want %q then the review id and %q", out, id, want)
	}
	if got, want := verdictFacts(t), map[string]any{"verdict.value": "fail", "verdict.plan_version": float64(1), "verdict.laws": float64(2)}; !reflect.DeepEqual(got, want) {
		t.Errorf("lit verdict event facts = %v, want %v", got, want)
	}
	if show := runCLIInDir(t, dir, "show", id); !strings.Contains(show, "reviews of plan v2: none") {
		t.Errorf("lit show counts a review of v1 as a review of the current v2:\n%s", show)
	}

	runCLIInDir(t, dir, "verdict", id, "pass", "--version", "2", "--by", "reviewer")
	show := runCLIInDir(t, dir, "show", id)
	if !strings.Contains(show, "\nplan (v2):\n"+second+"\nreviews of plan v2:\n- pass [reviewer @ ") || !strings.Contains(show, "] laws: -\n") {
		t.Errorf("lit show does not list the pass of v2 under the current plan:\n%s", show)
	}
	if strings.Contains(show, "fail") || strings.Contains(show, first) {
		t.Errorf("lit show prints the superseded plan or its review:\n%s", show)
	}

	history := runCLIInDir(t, dir, "history", id)
	steps := []string{
		"    plan: v1 → v2\n",
		"] verdict fail on plan v1 — laws: carrying-cost, one-source-of-truth\n",
		"] verdict pass on plan v2 — laws: -\n",
		"    plan: 2 versions — lit history " + id + " --version plan@<n> prints one\n",
	}
	at := 0
	for _, step := range steps {
		next := strings.Index(history[at:], step)
		if next < 0 {
			t.Fatalf("lit history lacks %q after offset %d, in the order the trail recorded it:\n%s", step, at, history)
		}
		at += next + len(step)
	}
	if strings.Contains(history, first) || strings.Contains(history, second) {
		t.Errorf("lit history prints plan text:\n%s", history)
	}

	if got := runCLIInDir(t, dir, "show", id, "--field", "plan"); got != second+"\n" {
		t.Errorf("lit show --field plan = %q, want the current plan", got)
	}
	if got := runCLIInDir(t, dir, "history", id, "--version", "plan@1"); got != first+"\n" {
		t.Errorf("lit history --version plan@1 = %q, want the first plan", got)
	}
}

// lit verdict refuses what is not a review of a recorded plan version, and
// says what it needs.
func TestVerdictRefusesWhatIsNotAReview(t *testing.T) {
	isolateEvents(t)
	dir, id, _ := statuslineCheckout(t)
	runCLIInDir(t, dir, "update", id, "--plan", "Cut the pass", "--by", "author")

	for _, refusal := range []struct {
		args []string
		want string
	}{
		{[]string{"pass"}, "usage: lit verdict <id> <pass|fail> --version <n>"},
		{[]string{"maybe", "--version", "1"}, `verdict must be pass or fail, got "maybe"`},
		{[]string{"fail", "--version", "1"}, "a failing verdict cites at least one law"},
		{[]string{"fail", "--version", "1", "--law", "One Source"}, `a law is its token, lower-case words joined by hyphens (one-source-of-truth), got "One Source"`},
		{[]string{"fail", "--version", "1", "--law", "a,b"}, `got "a,b"`},
		{[]string{"pass", "--version", "2"}, id + " has no plan@2: its record holds 1 version of plan"},
	} {
		args := append([]string{"verdict", id}, refusal.args...)
		out, err := runCLIInDirAllowError(t, dir, args...)
		if err == nil || !strings.Contains(out+err.Error(), refusal.want) {
			t.Errorf("lit %v = %q, %v; want a refusal naming %q", args, out, err, refusal.want)
		}
	}
	if show := runCLIInDir(t, dir, "show", id); !strings.Contains(show, "reviews of plan v1: none") {
		t.Errorf("a refused verdict was recorded:\n%s", show)
	}
}
