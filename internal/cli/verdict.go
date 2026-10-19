package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// verdictLeaf is `lit verdict <id> <pass|fail> --plan <digest> --law <token>`:
// a law review of one text of a ticket's plan, named by the digest `lit show`
// prints beside it, recorded by the reviewer. storage.PlanReviewOf parses
// what the reviewer gave. The reviewer resolves through the same identity rule
// as every actor. [LAW:single-enforcer]
func verdictLeaf() appLeaf {
	fs := newCobraFlagSet("verdict")
	plan := fs.String("plan", "", "The digest of the plan text the review judged, as `lit show` prints it beside the plan's version (required)")
	laws := fs.StringArray("law", "A law the verdict cites, by its token (one-source-of-truth); repeat for each law; a fail cites at least one")
	resolveActor := registerActor(fs)
	const usage = "usage: lit verdict <id> <pass|fail> --plan <digest> [--law <token>]..."
	return appLeaf{fs: fs, positionals: 2, usage: usage, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
		if len(positional) != 2 || *plan == "" {
			return UsageError{Message: usage}
		}
		review, err := ap.Store.AddPlanReview(ctx, storage.AddPlanReviewInput{
			IssueID:   positional[0],
			Plan:      *plan,
			Verdict:   positional[1],
			Laws:      *laws,
			CreatedBy: resolveActor(),
		})
		if err != nil {
			return err
		}
		telemetry.Set(ctx, "verdict.value", string(review.Verdict))
		telemetry.Set(ctx, "verdict.plan_digest", string(review.PlanDigest))
		telemetry.Set(ctx, "verdict.laws", len(review.Laws))
		_, err = fmt.Fprintf(stdout, "%s %s: %s on plan %s — laws: %s\n", review.IssueID, review.ID, review.Verdict, review.PlanDigest, joinLaws(review.Laws))
		return err
	}}
}
