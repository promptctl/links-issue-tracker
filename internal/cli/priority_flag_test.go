package cli

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// links-cli-bvko at the command surface: the value a read surface prints has to
// be a value the flag that set it accepts. `lit show` and the issue rows print
// model.Priority.String(), so that word is what these drive the flag with —
// which is also what fails the moment --priority goes back to an fs.Int, since
// pflag's strconv.ParseInt refuses "urgent" before lit's own gate runs.
//
// It quantifies over model.Priorities() and drives all three write commands
// rather than naming "urgent" once, because the defect was never about one word
// or one command: the flag was declared the same wrong way in new, followup and
// update. [LAW:behavior-not-structure] asserts the stored priority, not the
// parse call.
func TestPriorityFlagAcceptsEveryWordTheReadSurfacesPrint(t *testing.T) {
	ctx := context.Background()

	for _, want := range model.Priorities() {
		word := want.String()

		// Both spellings a read surface emits: the word from `lit show`, and the
		// decimal from `lit export` / the `lit import` payload.
		for _, spelling := range []string{word, strconv.Itoa(int(want))} {
			t.Run(word+"/"+spelling, func(t *testing.T) {
				ap := newTestCLIApp(t)

				var newOut bytes.Buffer
				if err := runNew(ctx, &newOut, ap, []string{
					"--title", "priority round trip",
					"--topic", "priority",
					"--type", "task",
					"--priority", spelling,
				}); err != nil {
					t.Fatalf("runNew(--priority %q) error = %v", spelling, err)
				}
				createdID := firstIssueID(t, newOut.String())
				created, err := ap.Store.GetIssue(ctx, createdID)
				if err != nil {
					t.Fatalf("GetIssue(%s) error = %v", createdID, err)
				}
				if created.Priority != want {
					t.Fatalf("runNew(--priority %q) stored priority %d, want %d", spelling, int(created.Priority), int(want))
				}

				// The round trip closes here: what the read surface prints for the
				// row that was just written must be the word we can hand back.
				printed := created.Priority.String()
				if printed != word {
					t.Fatalf("stored priority prints %q, want %q", printed, word)
				}
				if _, err := model.ParsePriorityName(printed); err != nil {
					t.Fatalf("the word the read surface printed (%q) is refused by the write gate: %v", printed, err)
				}

				var updateOut bytes.Buffer
				if err := runUpdate(ctx, &updateOut, ap, []string{createdID, "--priority", spelling}); err != nil {
					t.Fatalf("runUpdate(--priority %q) error = %v", spelling, err)
				}

				var followupOut bytes.Buffer
				if err := runFollowup(ctx, &followupOut, ap, []string{
					"--on", createdID,
					"--title", "priority follow-up",
					"--priority", spelling,
				}); err != nil {
					t.Fatalf("runFollowup(--priority %q) error = %v", spelling, err)
				}
				followupID := firstIssueID(t, followupOut.String())
				followup, err := ap.Store.GetIssue(ctx, followupID)
				if err != nil {
					t.Fatalf("GetIssue(%s) error = %v", followupID, err)
				}
				if followup.Priority != want {
					t.Fatalf("runFollowup(--priority %q) stored priority %d, want %d", spelling, int(followup.Priority), int(want))
				}
			})
		}
	}
}

// The ticket's second half. A flag value outside the domain is a deterministic
// refusal, so it must exit ExitValidation and must NOT draw the default
// "Retry the command" remediation — an agent that trusts remediation text over
// the error body loops forever on a parse failure no retry can change. Before
// this fix the refusal came from pflag's ParseInt as a bare error, which missed
// the validation_refused arm and got exactly that advice.
func TestPriorityFlagRefusesOutOfDomainValuesWithoutAdvisingRetry(t *testing.T) {
	ctx := context.Background()

	// "2" earns its place: the salvage normalizer coerces legacy 5-level values
	// to normal, and a live write must refuse rather than inherit that tolerance.
	for _, raw := range []string{"7", "2", "high", "", "urgent,normal"} {
		t.Run("value="+raw, func(t *testing.T) {
			ap := newTestCLIApp(t)

			var stdout bytes.Buffer
			err := runNew(ctx, &stdout, ap, []string{
				"--title", "refused",
				"--topic", "priority",
				"--type", "task",
				"--priority", raw,
			})
			if err == nil {
				t.Fatalf("runNew(--priority %q) succeeded; the value is outside the sealed domain", raw)
			}
			if code := ExitCode(err); code != ExitValidation {
				t.Fatalf("runNew(--priority %q) exit code = %d, want %d (a domain refusal, not a generic failure)", raw, code, ExitValidation)
			}
			reason := commandErrorReason(err)
			if reason != "validation_refused" {
				t.Fatalf("runNew(--priority %q) reason = %q, want validation_refused", raw, reason)
			}
			if rem := commandErrorRemediation(reason); strings.Contains(rem, "Retry the command") {
				t.Fatalf("runNew(--priority %q) remediation advises retrying a deterministic refusal: %q", raw, rem)
			}
			// The refusal has to name what to type instead.
			for _, p := range model.Priorities() {
				if !strings.Contains(err.Error(), p.String()) {
					t.Fatalf("refusal %q does not name the accepted value %q", err.Error(), p.String())
				}
			}
		})
	}
}

// The flag's default is the same value the store would have applied on its own,
// so omitting --priority is not a third spelling of the domain.
func TestPriorityFlagDefaultsToNormal(t *testing.T) {
	ctx := context.Background()
	ap := newTestCLIApp(t)

	var stdout bytes.Buffer
	if err := runNew(ctx, &stdout, ap, []string{
		"--title", "no priority flag",
		"--topic", "priority",
		"--type", "task",
	}); err != nil {
		t.Fatalf("runNew() error = %v", err)
	}
	created, err := ap.Store.GetIssue(ctx, firstIssueID(t, stdout.String()))
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if created.Priority != model.PriorityNormal {
		t.Fatalf("default priority = %d, want %d", int(created.Priority), int(model.PriorityNormal))
	}
}

// Flag help and the usage strings are derived from the vocabulary, so they
// cannot drift from the gate the way "Priority: 0=normal, 1=urgent" did while
// every read surface printed words.
func TestPriorityChoicesNamesExactlyTheVocabulary(t *testing.T) {
	got := priorityChoices()
	want := make([]string, 0, len(model.Priorities()))
	for _, p := range model.Priorities() {
		want = append(want, p.String())
		if !strings.Contains(got, p.String()) {
			t.Errorf("priorityChoices() = %q, missing %q", got, p.String())
		}
	}
	if got != strings.Join(want, "|") {
		t.Errorf("priorityChoices() = %q, want %q", got, strings.Join(want, "|"))
	}
}

// Guards the seam the fix depends on: parsePriorityFlag must answer in
// ValidationError, because that type is what routes a bad priority to
// ExitValidation and to the deterministic-refusal remediation.
func TestParsePriorityFlagAnswersInValidationError(t *testing.T) {
	if _, err := parsePriorityFlag("7"); err == nil {
		t.Fatal("parsePriorityFlag(\"7\") succeeded")
	} else if _, ok := err.(ValidationError); !ok {
		t.Fatalf("parsePriorityFlag(\"7\") error type = %T, want ValidationError", err)
	}
	got, err := parsePriorityFlag(" URGENT ")
	if err != nil {
		t.Fatalf("parsePriorityFlag(\" URGENT \") error = %v", err)
	}
	if got != model.PriorityUrgent {
		t.Fatalf("parsePriorityFlag(\" URGENT \") = %d, want %d", int(got), int(model.PriorityUrgent))
	}
}
