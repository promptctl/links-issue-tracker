package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

var (
	renderNow       = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	elsewhereHolder = model.NewAttribution("aaaaaaaabbbb", "ws-local")
	hereHolder      = model.NewAttribution("ccccccccdddd", "ws-local")
)

// laneWithProgress builds an epic-major lane of `total` members, the first
// `done` closed, and — if activeID is non-empty — the member right after
// them in_progress under that id; every remaining member is plain open.
func laneWithProgress(t *testing.T, done, total int, activeID string) (claims.Evidence, model.LaneID) {
	t.Helper()
	epic := model.Issue{ID: "E", IssueType: model.TypeEpic}
	children := make([]model.Issue, 0, total)
	for i := 0; i < total; i++ {
		id := "leaf" + string(rune('0'+i))
		state := model.StateOpen
		switch {
		case i < done:
			state = model.StateClosed
		case activeID != "" && i == done:
			id, state = activeID, model.StateInProgress
		}
		issue, err := model.HydrateStatus(model.Issue{ID: id, Lane: "lane", IssueType: model.TypeTask}, model.StatusView{Value: state})
		if err != nil {
			t.Fatalf("hydrate %s: %v", id, err)
		}
		children = append(children, issue)
	}
	epicHydrated, err := model.HydrateAllOf(epic, children)
	if err != nil {
		t.Fatalf("hydrate epic: %v", err)
	}
	issues := append([]model.Issue{epicHydrated}, children...)
	parents := map[string]*model.Issue{}
	for _, c := range children {
		parents[c.ID] = &epicHydrated
	}
	evidence, err := claims.NewEvidence(issues, parents, nil)
	if err != nil {
		t.Fatalf("NewEvidence: %v", err)
	}
	lane := model.LaneOf(children[0], &epicHydrated)
	return evidence, lane
}

// TestFormatClaimLineUnclaimedLaneRendersNothing is the zero state: a lane
// nobody holds carries no claim line, exactly as an Unclaimed lane routes
// exactly as it always did pre-claims.
func TestFormatClaimLineUnclaimedLaneRendersNothing(t *testing.T) {
	evidence, lane := laneWithProgress(t, 0, 1, "")
	cc := claimContext{standings: claims.Standings{}, evidence: evidence}
	if _, ok := formatClaimLine(cc, lane, renderNow); ok {
		t.Fatalf("formatClaimLine on an Unclaimed lane rendered a line; want none")
	}
}

// TestFormatClaimLineDossierNeedsNoLocalAddress is the ticket's acceptance
// criterion, half one: the same claimed lane renders the full dossier
// (holder badge, freshness, lane progress) from cc.standings and cc.evidence
// alone — no cc.addresses entry required, which is what a second clone that
// only ever synced the shared database would have.
func TestFormatClaimLineDossierNeedsNoLocalAddress(t *testing.T) {
	evidence, lane := laneWithProgress(t, 1, 2, "active-ticket")
	standings := claims.Standings{lane: claims.Held{Tenure: claims.Tenure{
		By: elsewhereHolder, Since: renderNow.Add(-3 * time.Hour), LastActivity: renderNow.Add(-2 * time.Hour),
	}}}
	cc := claimContext{standings: standings, evidence: evidence} // no addresses

	line, ok := formatClaimLine(cc, lane, renderNow)
	if !ok {
		t.Fatalf("formatClaimLine on a Held lane returned ok=false")
	}
	if !strings.Contains(line, "elsewhere") {
		t.Fatalf("line = %q, want it to say the holder is elsewhere (no local address known)", line)
	}
	if strings.Contains(line, "/") && !strings.Contains(line, "1/2 done") {
		t.Fatalf("line = %q, want lane progress 1/2 done", line)
	}
	if !strings.Contains(line, "active-ticket in progress") {
		t.Fatalf("line = %q, want the active member named", line)
	}
	if !strings.Contains(line, "2 hours ago") {
		t.Fatalf("line = %q, want the freshness phrase", line)
	}
}

// TestFormatClaimLineAddressOnlyOnClaimantsOwnMachine is the acceptance
// criterion's other half: the same claimed lane's path/branch render only
// when cc.addresses resolves the holder to a live local worktree — never
// invented, and never shown for a holder this machine cannot prove is here.
func TestFormatClaimLineAddressOnlyOnClaimantsOwnMachine(t *testing.T) {
	evidence, lane := laneWithProgress(t, 0, 1, "")
	standings := claims.Standings{lane: claims.Held{Tenure: claims.Tenure{
		By: hereHolder, Since: renderNow.Add(-time.Hour), LastActivity: renderNow.Add(-time.Hour),
	}}}
	cc := claimContext{
		standings: standings,
		evidence:  evidence,
		addresses: map[model.Attribution]workspace.Checkout{
			hereHolder: {Path: "../links-wt-pgct", Branch: "links-claims-1ihf.11"},
		},
	}

	line, ok := formatClaimLine(cc, lane, renderNow)
	if !ok {
		t.Fatalf("formatClaimLine on a Held lane returned ok=false")
	}
	if !strings.Contains(line, "claimed here: ../links-wt-pgct (links-claims-1ihf.11)") {
		t.Fatalf("line = %q, want the resolved local address", line)
	}

	// The same holder, on a machine that never enumerated (or that isn't
	// this holder's machine at all) gets the dossier and nothing more.
	remote := claimContext{standings: standings, evidence: evidence}
	remoteLine, ok := formatClaimLine(remote, lane, renderNow)
	if !ok {
		t.Fatalf("formatClaimLine on a Held lane (no addresses) returned ok=false")
	}
	if strings.Contains(remoteLine, "../links-wt-pgct") {
		t.Fatalf("remoteLine = %q, must not carry a path this machine never resolved", remoteLine)
	}
	if !strings.Contains(remoteLine, "elsewhere") {
		t.Fatalf("remoteLine = %q, want the opaque elsewhere badge", remoteLine)
	}
}

// TestFormatClaimLineStaleHolderStillResolvesALiveAddress: a claim can go
// stale while its worktree is still very much alive, and "go look at what it
// was doing" is exactly as true then as for a fresh claim.
func TestFormatClaimLineStaleHolderStillResolvesALiveAddress(t *testing.T) {
	evidence, lane := laneWithProgress(t, 0, 1, "")
	standings := claims.Standings{lane: claims.Stale{Tenure: claims.Tenure{
		By: hereHolder, Since: renderNow.Add(-72 * time.Hour), LastActivity: renderNow.Add(-49 * time.Hour),
	}}}
	cc := claimContext{
		standings: standings,
		evidence:  evidence,
		addresses: map[model.Attribution]workspace.Checkout{
			hereHolder: {Path: "../links-wt-pgct", Branch: ""},
		},
	}
	line, ok := formatClaimLine(cc, lane, renderNow)
	if !ok {
		t.Fatalf("formatClaimLine on a Stale lane returned ok=false")
	}
	if !strings.Contains(line, "claimed here (stale): ../links-wt-pgct (detached HEAD)") {
		t.Fatalf("line = %q, want a stale-tagged local address with detached HEAD rendered", line)
	}
}

// TestDescribeClaimantNamesAnUnaddressableHolder pins the transfer notice's
// half of the public-checkout ruling: the four renderings describeClaimant can
// produce, each against the exact string a reader sees. The unattributed row is
// driven through the notice it appears in by
// TestTransferNoticeNamesAPredecessorThatMintedNoToken.
//
// The empty-assignee rows are the ruling: the old text read "(unassigned)",
// which described the empty field while saying nothing about the holder being
// announced -- and the record carries an establishing event, so somebody
// demonstrably took this ticket.
func TestDescribeClaimantNamesAnUnaddressableHolder(t *testing.T) {
	for _, row := range []struct {
		name     string
		claimant claims.Claimant
		want     string
	}{
		{
			name:     "no assignee and no token: the bucket is the whole answer",
			claimant: claims.Claimant{Established: true},
			want:     "the public checkout",
		},
		{
			name:     "no assignee, identified checkout: the token carries it alone",
			claimant: claims.Claimant{Established: true, Checkout: elsewhereHolder},
			want:     "stream aaaaaaaa",
		},
		{
			name:     "assignee and no token: the bucket discriminates nothing beside a name",
			claimant: claims.Claimant{Established: true, Assignee: "alpha-agent"},
			want:     "alpha-agent",
		},
		{
			name:     "both halves: either one can be the half that moved",
			claimant: claims.Claimant{Established: true, Assignee: "alpha-agent", Checkout: elsewhereHolder},
			want:     "alpha-agent (stream aaaaaaaa)",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := describeClaimant(row.claimant); got != row.want {
				t.Fatalf("describeClaimant(%+v) = %q, want %q", row.claimant, got, row.want)
			}
		})
	}
}

// TestTransferNoticeNamesAPredecessorThatMintedNoToken takes a ticket over from
// a hold recorded with no stream token and reads the notice a reader actually
// sees: the public checkout, with no assignee beside it.
func TestTransferNoticeNamesAPredecessorThatMintedNoToken(t *testing.T) {
	h := newReadyTestHarness(t)
	issue := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: "unattributed hold", Topic: "claims", IssueType: "task"})
	h.asCheckout("")
	h.transition(issue.ID, model.Start{})

	notice, err := transferNotice(h.ctx, h.ap, issue.ID, model.Start{Assignee: "bravo-agent"})
	if err != nil {
		t.Fatalf("transferNotice error = %v", err)
	}
	want := fmt.Sprintf("claim transferred: the public checkout -> bravo-agent (%s)\n", nameCheckout(ownAttribution(h.ap)))
	if notice != want {
		t.Fatalf("transferNotice = %q, want %q", notice, want)
	}
}

// TestFormatClaimLinePublicCheckoutIsNeverHere pins the rendering half of the
// public-checkout ruling, and the regression that deleting claimPrefix's self
// arm was meant to close.
//
// Every other standing literal in this file names a holder with a real stream
// token, so none of them could reach the branch an unattributed holder takes.
// A zero Attribution is both a legitimate holder and the zero value of
// claimContext.self, and the two coincide on exactly the reports that build a
// context carrying no Stream at all -- `lit sync`'s contested-lane report among
// them. The old arm read that coincidence as proof of ownership and announced
// a foreign lane as "claimed here: this checkout".
//
// So cc.self varies down the rows and the expected badge does not: rendering
// reads the holder and the addresses this machine actually resolved, never who
// is asking. An identified self must not make the public lane foreign, and an
// absent self must not make it ours.
func TestFormatClaimLinePublicCheckoutIsNeverHere(t *testing.T) {
	publicHolder := model.Attribution{}
	for _, row := range []struct {
		name     string
		self     model.Attribution
		standing claims.Standing
		want     string
	}{
		{
			name:     "held, asked by an identified checkout",
			self:     hereHolder,
			standing: claims.Held{Tenure: claims.Tenure{By: publicHolder, LastActivity: renderNow.Add(-2 * time.Hour)}},
			want:     "claimed: the public checkout (unaddressed)",
		},
		{
			name:     "held, asked by a checkout that minted no token of its own",
			self:     publicHolder,
			standing: claims.Held{Tenure: claims.Tenure{By: publicHolder, LastActivity: renderNow.Add(-2 * time.Hour)}},
			want:     "claimed: the public checkout (unaddressed)",
		},
		{
			name:     "stale, asked by a checkout that minted no token of its own",
			self:     publicHolder,
			standing: claims.Stale{Tenure: claims.Tenure{By: publicHolder, LastActivity: renderNow.Add(-49 * time.Hour)}},
			want:     "claimed: the public checkout (stale)",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			evidence, lane := laneWithProgress(t, 0, 1, "")
			cc := claimContext{standings: claims.Standings{lane: row.standing}, evidence: evidence, self: row.self}

			line, ok := formatClaimLine(cc, lane, renderNow)
			if !ok {
				t.Fatalf("formatClaimLine on a %T lane returned ok=false", row.standing)
			}
			if !strings.Contains(line, row.want) {
				t.Fatalf("line = %q, want it to contain %q", line, row.want)
			}
			if strings.Contains(line, "claimed here") {
				t.Fatalf("line = %q, must never announce the public checkout as this one", line)
			}
		})
	}
}

// TestFormatClaimLineContestedAppendsContestants: contest is an annotation
// on a Held lane, not a routing decision — the line names every contestant
// alongside the holder.
func TestFormatClaimLineContestedAppendsContestants(t *testing.T) {
	evidence, lane := laneWithProgress(t, 0, 1, "")
	contestant := model.NewAttribution("eeeeeeeeffff", "ws-local")
	standings := claims.Standings{lane: claims.Held{
		Tenure:    claims.Tenure{By: elsewhereHolder, LastActivity: renderNow},
		Contested: []model.Attribution{contestant},
	}}
	cc := claimContext{standings: standings, evidence: evidence}
	line, ok := formatClaimLine(cc, lane, renderNow)
	if !ok {
		t.Fatalf("formatClaimLine on a contested lane returned ok=false")
	}
	if !strings.Contains(line, "contested by "+nameCheckout(contestant)) {
		t.Fatalf("line = %q, want the contestant named", line)
	}
}
