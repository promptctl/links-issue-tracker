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
func laneWithProgress(t *testing.T, done, total int, activeID model.ID) (claims.Evidence, model.LaneID) {
	t.Helper()
	epic := model.Issue{ID: "E", IssueType: model.TypeEpic}
	children := make([]model.Issue, 0, total)
	for i := 0; i < total; i++ {
		id := model.ID("leaf" + string(rune('0'+i)))
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
	parents := map[model.ID]model.ID{}
	for _, c := range children {
		parents[c.ID] = epicHydrated.ID
	}
	evidence, err := claims.NewEvidence(issues, parents, nil)
	if err != nil {
		t.Fatalf("NewEvidence: %v", err)
	}
	lane := model.LaneOf(children[0], &epicHydrated)
	return evidence, lane
}

// TestFormatClaimLineUnclaimedLaneRendersNothing is the zero state: a lane
// nobody holds carries no claim line.
func TestFormatClaimLineUnclaimedLaneRendersNothing(t *testing.T) {
	t.Parallel()
	evidence, lane := laneWithProgress(t, 0, 1, "")
	cc := claimContext{standings: claims.Standings{}, evidence: evidence, judgedAt: renderNow}
	if _, ok := formatClaimLine(cc, lane); ok {
		t.Fatalf("formatClaimLine on an Unclaimed lane rendered a line; want none")
	}
}

// TestFormatClaimLineDossierNeedsNoLocalAddress is the ticket's acceptance
// criterion, half one: the same claimed lane renders the full dossier
// (holder badge, freshness, lane progress) from cc.standings and cc.evidence
// alone — no cc.addresses entry required, which is what a second clone that
// only ever synced the shared database would have.
func TestFormatClaimLineDossierNeedsNoLocalAddress(t *testing.T) {
	t.Parallel()
	evidence, lane := laneWithProgress(t, 1, 2, "active-ticket")
	standings := claims.Standings{lane: claims.Held{Tenure: claims.Tenure{
		By: elsewhereHolder, Since: renderNow.Add(-3 * time.Hour), LastActivity: renderNow.Add(-2 * time.Hour),
	}}}
	cc := claimContext{standings: standings, evidence: evidence, judgedAt: renderNow} // no addresses

	line, ok := formatClaimLine(cc, lane)
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

// TestFormatClaimLineNamesOnlyTheCheckout: the holder is the checkout, so the
// line names the checkout's address and nobody acting in it.
func TestFormatClaimLineNamesOnlyTheCheckout(t *testing.T) {
	t.Parallel()
	evidence, lane := laneWithProgress(t, 0, 1, "")
	cc := claimContext{
		judgedAt:  renderNow,
		standings: claims.Standings{lane: claims.Held{Tenure: claims.Tenure{By: hereHolder, LastActivity: renderNow}}},
		evidence:  evidence,
		addresses: map[model.Attribution]workspace.Checkout{hereHolder: {Path: "../wt", Branch: "topic"}},
	}
	line, ok := formatClaimLine(cc, lane)
	if !ok {
		t.Fatalf("formatClaimLine on a Held lane returned ok=false")
	}
	if want := "claimed here: ../wt (topic) · "; !strings.HasPrefix(line, want) {
		t.Fatalf("claim line = %q, want it to open with %q", line, want)
	}
}

// TestFormatClaimLineAddressOnlyOnClaimantsOwnMachine is the acceptance
// criterion's other half: the same claimed lane's path/branch render only
// when cc.addresses resolves the holder to a live local worktree — never
// invented, and never shown for a holder this machine cannot prove is here.
func TestFormatClaimLineAddressOnlyOnClaimantsOwnMachine(t *testing.T) {
	t.Parallel()
	evidence, lane := laneWithProgress(t, 0, 1, "")
	standings := claims.Standings{lane: claims.Held{Tenure: claims.Tenure{
		By: hereHolder, Since: renderNow.Add(-time.Hour), LastActivity: renderNow.Add(-time.Hour),
	}}}
	cc := claimContext{
		judgedAt:  renderNow,
		standings: standings,
		evidence:  evidence,
		addresses: map[model.Attribution]workspace.Checkout{
			hereHolder: {Path: "../links-wt-pgct", Branch: "links-claims-1ihf.11"},
		},
	}

	line, ok := formatClaimLine(cc, lane)
	if !ok {
		t.Fatalf("formatClaimLine on a Held lane returned ok=false")
	}
	if !strings.Contains(line, "claimed here: ../links-wt-pgct (links-claims-1ihf.11)") {
		t.Fatalf("line = %q, want the resolved local address", line)
	}

	// The same holder, on a machine that never enumerated (or that isn't
	// this holder's machine at all) gets the dossier and nothing more.
	remote := claimContext{standings: standings, evidence: evidence, judgedAt: renderNow}
	remoteLine, ok := formatClaimLine(remote, lane)
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

// TestFormatClaimLineExpiredClaimRendersNothingEvenWithAnAddress: the holder's
// worktree is still on this machine and resolvable, but the claim on the lane
// has expired, so the lane is Unclaimed and nothing is printed — not the
// address, not the age, not a badge. An expired claim is not a claim to print.
// The address map is populated on purpose: a renderer that consulted it before
// the standing would still find something to say.
func TestFormatClaimLineExpiredClaimRendersNothingEvenWithAnAddress(t *testing.T) {
	t.Parallel()
	evidence, lane := laneWithProgress(t, 0, 1, "")
	cc := claimContext{
		judgedAt:  renderNow,
		standings: claims.Standings{lane: claims.Unclaimed{}},
		evidence:  evidence,
		addresses: map[model.Attribution]workspace.Checkout{
			hereHolder: {Path: "../links-wt-pgct", Branch: ""},
		},
	}
	if line, ok := formatClaimLine(cc, lane); ok || line != "" {
		t.Fatalf("formatClaimLine on an unclaimed lane with a resolvable address = (%q, %v), want nothing: there is no claim to describe", line, ok)
	}
}

// TestCheckoutAddressNamesAHolder pins how every holder is named — claim
// line, refusal, transfer notice: its checkout's address when this machine has
// one, the opaque name otherwise.
func TestCheckoutAddressNamesAHolder(t *testing.T) {
	t.Parallel()
	addresses := map[model.Attribution]workspace.Checkout{elsewhereHolder: {Path: "../wt", Branch: "topic"}}
	for _, row := range []struct {
		name   string
		holder model.Attribution
		want   string
	}{
		{"no token", model.Attribution{}, "the public checkout"},
		{"an addressable checkout", elsewhereHolder, "../wt (topic)"},
		{"a checkout this machine cannot address", model.NewAttribution("bbbbbbbbbbbb", "ws"), "stream bbbbbbbb"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := checkoutAddress(row.holder, addresses); got != row.want {
				t.Fatalf("checkoutAddress(%+v) = %q, want %q", row.holder, got, row.want)
			}
		})
	}
}

// TestTransferNoticeNamesAPredecessorThatMintedNoToken takes a ticket over from
// a hold recorded with no stream token and reads the notice a reader actually
// sees for the handover Apply reports: the public checkout, then this one.
func TestTransferNoticeNamesAPredecessorThatMintedNoToken(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	epic := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("Home epic"), Lane: storage.SharedLane, Topic: "claims", IssueType: "epic"})
	issue := h.createIssue(storage.CreateIssueInput{Prefix: "test", Title: storage.MustParseTitle("unattributed hold"), Lane: storage.SharedLane, Topic: "claims", IssueType: "task", ParentID: epic.ID})
	h.asCheckout("")
	h.transition(issue.ID, model.Start{})

	h.asCheckout(h.ap.Stream.Value())
	applied, err := h.ap.Store.Apply(h.ctx, issue.ID, storage.Change{Action: model.Start{Assignee: "bravo-agent"}, Actor: "bravo-agent"})
	if err != nil {
		t.Fatalf("Apply takeover error = %v", err)
	}
	// The standing the gate read before Apply: the unattributed start holds the
	// lane, so its holder is the handover's predecessor.
	predecessor := applied.Handover.From.Checkout
	if predecessor.Present() {
		t.Fatalf("predecessor checkout = %v, want the public checkout", predecessor)
	}
	notice := transferNotice(claims.Held{Tenure: claims.Tenure{By: predecessor}}, applied.Handover, nil)
	want := fmt.Sprintf("claim transferred: the public checkout -> %s\n", nameCheckout(ownAttribution(h.ap)))
	if notice != want {
		t.Fatalf("transferNotice = %q, want %q", notice, want)
	}
}

// TestFormatClaimLinePublicCheckoutIsNeverHere pins the rendering half of the
// public-checkout ruling.
//
// Every other standing literal in this file names a holder with a real stream
// token, so none of them could reach the branch an unattributed holder takes.
// A zero Attribution is both a legitimate holder and the zero value of
// claimContext.self, and the two coincide on exactly the reports that build a
// context carrying no Stream at all. Reading that coincidence as proof of
// ownership would announce a foreign lane as "claimed here: this checkout".
//
// So cc.self varies down the rows and the expected badge does not: rendering
// reads the holder and the addresses this machine actually resolved, never who
// is asking. An identified self must not make the public lane foreign, and an
// absent self must not make it ours.
func TestFormatClaimLinePublicCheckoutIsNeverHere(t *testing.T) {
	t.Parallel()
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
			self:     model.Attribution{},
			standing: claims.Held{Tenure: claims.Tenure{By: publicHolder, LastActivity: renderNow.Add(-2 * time.Hour)}},
			want:     "claimed: the public checkout (unaddressed)",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			evidence, lane := laneWithProgress(t, 0, 1, "")
			cc := claimContext{standings: claims.Standings{lane: row.standing}, evidence: evidence, self: row.self, judgedAt: renderNow}

			line, ok := formatClaimLine(cc, lane)
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

// A claim line measures its holder's age from the instant the claim context
// was judged at, and from nothing else: the same standing reads two hours old
// at one judged instant and three days old at another.
func TestFormatClaimLineMeasuresAgeFromTheJudgedInstant(t *testing.T) {
	t.Parallel()
	evidence, lane := laneWithProgress(t, 0, 1, "")
	standings := claims.Standings{lane: claims.Held{Tenure: claims.Tenure{By: elsewhereHolder, LastActivity: renderNow}}}
	for _, tc := range []struct {
		judgedAt time.Time
		want     string
	}{
		{renderNow.Add(2 * time.Hour), "2 hours ago"},
		{renderNow.Add(72 * time.Hour), "3 days ago"},
	} {
		line, ok := formatClaimLine(claimContext{standings: standings, evidence: evidence, judgedAt: tc.judgedAt}, lane)
		if !ok {
			t.Fatalf("formatClaimLine on a Held lane returned ok=false")
		}
		if !strings.Contains(line, tc.want) {
			t.Fatalf("judged at %s: line = %q, want it to say %q", tc.judgedAt, line, tc.want)
		}
	}
}

// A holder's age reads as how long before the judged instant it last acted,
// and activity recorded after that instant says it is in the future instead
// of passing for activity a moment ago.
func TestActivityAgeSaysWhenActivityIsAheadOfTheJudgedInstant(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		lastActivity time.Time
		want         string
	}{
		{"behind", renderNow.Add(-3 * time.Hour), "3 hours ago"},
		{"at", renderNow, "under a minute ago"},
		{"ahead", renderNow.Add(5 * time.Minute), "5 minutes in the future"},
		{"just ahead", renderNow.Add(time.Nanosecond), "under a minute in the future"},
	} {
		if got := activityAge(tc.lastActivity, renderNow); got != tc.want {
			t.Errorf("%s: activityAge = %q, want %q", tc.name, got, tc.want)
		}
	}
}
