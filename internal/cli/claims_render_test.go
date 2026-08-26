package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/claims"
	"github.com/promptctl/links-issue-tracker/internal/model"
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
	if !strings.Contains(line, "contested by "+shortStream(contestant)) {
		t.Fatalf("line = %q, want the contestant named", line)
	}
}
