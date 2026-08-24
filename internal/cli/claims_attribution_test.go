package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/model"
)

// checkoutIdentity reports the attribution pair a checkout stamps its own work
// with, read through the same read-mode open any command uses — never by
// reaching into the token file, so the test learns the identity the way the
// program does.
func checkoutIdentity(t *testing.T, dir string) model.Attribution {
	t.Helper()
	ap, err := app.Open(context.Background(), dir, app.AccessRead)
	if err != nil {
		t.Fatalf("app.Open(%s) error = %v", dir, err)
	}
	defer ap.Close()
	pair := model.NewAttribution(ap.Stream.Value(), ap.Workspace.WorkspaceID)
	if !pair.Present() {
		t.Fatalf("checkout %s reports no identity; it has already mutated, so one must exist", dir)
	}
	return pair
}

// eventsByAttribution groups every event in a checkout's store by the pair it
// carries, which is the whole question this feature answers: given a shared
// backlog, which checkout produced which work.
func eventsByAttribution(t *testing.T, dir string) map[model.Attribution][]model.IssueEvent {
	t.Helper()
	ap, err := app.Open(context.Background(), dir, app.AccessRead)
	if err != nil {
		t.Fatalf("app.Open(%s) error = %v", dir, err)
	}
	defer ap.Close()
	dump, err := ap.Store.Export(context.Background())
	if err != nil {
		t.Fatalf("Export(%s) error = %v", dir, err)
	}
	if len(dump.Events) == 0 {
		t.Fatalf("checkout %s holds no events — the fixture proves nothing", dir)
	}
	grouped := map[model.Attribution][]model.IssueEvent{}
	for _, event := range dump.Events {
		grouped[event.Attribution] = append(grouped[event.Attribution], event)
	}
	return grouped
}

// TestSyncCarriesAttributionToASecondClone is the feature's acceptance proof:
// work done in one clone arrives in another still carrying the pair that says
// which checkout did it. Everything downstream in the claims design — the
// predicate, contest detection, routing `next` around another checkout's lane —
// is a reading of these stamps, so if they do not survive the trip there is
// nothing to read.
//
// The two clones are driven through the real CLI over a real git remote rather
// than by copying rows, because the claim being tested is about what survives
// sync, and an in-process fixture would assert the value never left.
func TestSyncCarriesAttributionToASecondClone(t *testing.T) {
	base := t.TempDir()
	runGit(t, base, "init", "--bare", "remote.git")
	remote := filepath.Join(base, "remote.git")

	producer := filepath.Join(base, "alpha")
	runGit(t, base, "clone", remote, "alpha")
	runGit(t, producer, "config", "user.email", "a@a.co")
	runGit(t, producer, "config", "user.name", "alpha")
	if err := os.WriteFile(filepath.Join(producer, "readme.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write readme error = %v", err)
	}
	runGit(t, producer, "add", "-A")
	runGit(t, producer, "commit", "-m", "seed")
	runGit(t, producer, "push", "origin", "HEAD")
	runCLIInDir(t, producer, "init", "--skip-hooks", "--skip-agents")

	// Creation and a lifecycle transition: the two event kinds the claim
	// predicate treats as establishing, so both must arrive attributed.
	producerTicket := extractTicketID(t, runCLIInDir(t, producer,
		"new", "--title", "producer-ticket", "--topic", "demo", "--type", "task"))
	runCLIInDir(t, producer, "start", producerTicket)
	runCLIInDir(t, producer, "sync", "push", "--set-upstream")

	producerPair := checkoutIdentity(t, producer)

	consumer := filepath.Join(base, "bravo")
	runGit(t, base, "clone", remote, "bravo")
	runGit(t, consumer, "config", "user.email", "b@b.co")
	runGit(t, consumer, "config", "user.name", "bravo")
	runCLIInDir(t, consumer, "init", "--skip-hooks", "--skip-agents")

	// The consumer sees the producer's stamps, unchanged by the trip. Asserting
	// on the exact pair — not merely that something is present — is what makes
	// this fail if adoption ever re-stamped inherited history as its own.
	inherited := eventsByAttribution(t, consumer)
	if len(inherited[producerPair]) == 0 {
		t.Fatalf("consumer holds no events attributed to the producer %+v; it sees %v",
			producerPair, attributionsOf(inherited))
	}
	for pair, events := range inherited {
		if pair != producerPair {
			t.Errorf("consumer holds %d event(s) attributed to %+v, but every event it has was produced by %+v",
				len(events), pair, producerPair)
		}
	}

	// The payoff: the consumer's own work is distinguishable from the
	// producer's in the one shared backlog. Two checkouts, two identities, no
	// coordination between them beyond the database itself.
	runCLIInDir(t, consumer, "new", "--title", "consumer-ticket", "--topic", "demo", "--type", "task")
	consumerPair := checkoutIdentity(t, consumer)
	if consumerPair == producerPair {
		t.Fatalf("both checkouts report the same identity %+v — their work is indistinguishable", consumerPair)
	}

	mixed := eventsByAttribution(t, consumer)
	if len(mixed[consumerPair]) == 0 {
		t.Errorf("the consumer's own new ticket is not attributed to it (%+v); store holds %v",
			consumerPair, attributionsOf(mixed))
	}
	if len(mixed[producerPair]) == 0 {
		t.Errorf("the producer's events lost their attribution once the consumer wrote to the store")
	}
}

// attributionsOf renders the pairs present in a grouping, so a failure names
// what the store actually held instead of only what it lacked.
func attributionsOf(grouped map[model.Attribution][]model.IssueEvent) []model.Attribution {
	out := make([]model.Attribution, 0, len(grouped))
	for pair := range grouped {
		out = append(out, pair)
	}
	return out
}
