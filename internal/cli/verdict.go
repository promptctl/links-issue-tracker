package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// verdictLeaf is `lit verdict <id> <pass|fail> --version <n> --law <token>`:
// a law review of one version of a ticket's plan, recorded by the reviewer.
// The reviewer resolves through the same identity rule as every actor, so lit
// records who gave the verdict rather than taking anyone's word for it.
// [LAW:single-enforcer]
func verdictLeaf() appLeaf {
	fs := newCobraFlagSet("verdict")
	version := fs.Int("version", 0, "The plan version the review judged, as `lit show` and `lit history` number it (required)")
	laws := fs.StringArray("law", "A law the verdict cites, by its token (one-source-of-truth); repeat for each law; a fail cites at least one")
	resolveActor := registerActor(fs)
	const usage = "usage: lit verdict <id> <pass|fail> --version <n> [--law <token>]..."
	return appLeaf{fs: fs, positionals: 2, usage: usage, work: func(ctx context.Context, stdout io.Writer, ap *app.App, positional []string) error {
		// A version counts from 1, so an absent --version is the zero no
		// version is numbered by.
		if len(positional) != 2 || *version < 1 {
			return UsageError{Message: usage}
		}
		verdict, err := model.ParsePlanVerdict(positional[1])
		if err != nil {
			return err
		}
		cited := []model.Law{}
		for _, raw := range *laws {
			law, err := model.ParseLaw(raw)
			if err != nil {
				return err
			}
			cited = append(cited, law)
		}
		review, err := ap.Store.AddPlanReview(ctx, storage.AddPlanReviewInput{
			IssueID:   positional[0],
			Version:   *version,
			Verdict:   verdict,
			Laws:      cited,
			CreatedBy: resolveActor(),
		})
		if err != nil {
			return err
		}
		telemetry.Set(ctx, "verdict.value", string(review.Verdict))
		telemetry.Set(ctx, "verdict.plan_version", *version)
		telemetry.Set(ctx, "verdict.laws", len(review.Laws))
		_, err = fmt.Fprintf(stdout, "%s %s: %s on plan v%d — laws: %s\n", review.IssueID, review.ID, review.Verdict, *version, joinLaws(review.Laws))
		return err
	}}
}
