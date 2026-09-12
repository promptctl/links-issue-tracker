package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/version"
)

// stampBuild rewrites the process-global link-time version vars for the length
// of one test and restores them after. Tests using it must not run in parallel:
// the vars are process-wide, and runCLIInDir chdirs besides.
func stampBuild(t *testing.T, origin, date string) {
	t.Helper()
	origV, origD, origO := version.Version, version.Date, version.Origin
	t.Cleanup(func() { version.Version, version.Date, version.Origin = origV, origD, origO })
	version.Date = date
	version.Origin = origin
}

// bannerReadCommand is one invocation that carries the staleness banner.
type bannerReadCommand struct {
	name string
	args []string
}

// bannerReadCommands enumerates every read command wired to
// printStalenessWarning — the three call sites in next.go, workable.go and
// cli.go's showLeaf. It has one home because the tests below used to list
// `next` and `backlog` each for themselves and omit `show` from both, and a
// banner claim checked at two of its three sites is how the line came to say
// "including the routing behind this answer" on a command that routes nothing.
// A fourth wiring adds one row here and every test below covers it at once.
func bannerReadCommands(ticketID string) []bannerReadCommand {
	return []bannerReadCommand{
		{"next", []string{"next"}},
		{"backlog", []string{"backlog"}},
		{"show", []string{"show", ticketID}},
	}
}

// TestReadCommandsAnnounceAStaleBuild is acceptance criterion 1 of
// links-build-status-1svs: the commands an agent runs every session — and
// `next` is the one that makes a routing DECISION — say so when the binary
// producing that answer predates the tree it was built from. Before this, they
// printed their sync-staleness warning in exactly the right position and said
// nothing about the binary, and a session spent a full investigation inside
// routing code that was already correct.
func TestReadCommandsAnnounceAStaleBuild(t *testing.T) {
	stampBuild(t, version.OriginSource, time.Now().Add(-10*24*time.Hour).UTC().Format(time.RFC3339))
	// A workspace with a real ticket in it, so `next` reaches an actual routing
	// decision rather than an empty-backlog refusal — the decision is what the
	// ticket says must announce the binary behind it. The ticket is also what
	// makes `lit show` reachable as itself rather than as an id-not-found error.
	dir, ticketID := unpushedCloneWithOneLocalChange(t)

	for _, cmd := range bannerReadCommands(ticketID) {
		out := runCLIInDir(t, dir, cmd.args...)
		if !strings.Contains(out, "built 10 days ago") {
			t.Errorf("lit %s did not announce the stale build:\n%s", cmd.name, out)
		}
		if !strings.Contains(out, "just install") {
			t.Errorf("lit %s announced staleness without naming the remedy:\n%s", cmd.name, out)
		}
	}
}

// TestStaleBuildWarningClaimsNothingCommandSpecific is the pin the routing
// clause got past. One banner is shared by three read commands, so its claim
// must be true of all three: `lit show <id>` performs no routing — the caller
// names the ticket — and a line asserting "the routing behind this answer"
// there points a reader at a concern that does not exist on that command.
//
// Two arms, and they are not the same check. The word ban is a named tripwire
// for the regression that actually happened, and nothing more: it is a loose
// match, and a line saying "the ticket selection behind this answer" would walk
// straight past it. The sameness check is the general contract — one shared
// banner may make only a claim it can make everywhere, so the three sites must
// render one identical line, and specializing the text per command reddens this
// and makes the specializer prove the new claim at every site it reaches.
//
// MEASURED, not assumed. Restoring the routing clause fails this test naming
// every site (1 `--- FAIL`, counted rather than read off the exit code, since
// `go test` runs vet and a mutation that will not compile reports zero
// failures). Worth recording that the word ban alone did NOT need the `show`
// row to bite — it reddens on `next` and `backlog` too — so the row's value is
// coverage of a site nothing exercised, which is what let the clause ship, and
// not a mutation this arm catches.
func TestStaleBuildWarningClaimsNothingCommandSpecific(t *testing.T) {
	stampBuild(t, version.OriginSource, time.Now().Add(-10*24*time.Hour).UTC().Format(time.RFC3339))
	dir, ticketID := unpushedCloneWithOneLocalChange(t)

	var first, firstName string
	for _, cmd := range bannerReadCommands(ticketID) {
		line := buildLineOf(t, runCLIInDir(t, dir, cmd.args...))
		if strings.Contains(line, "routing") {
			t.Errorf("lit %s names routing, but this banner is shared with `lit show`, which routes nothing:\n%s", cmd.name, line)
		}
		if first == "" {
			first, firstName = line, cmd.name
			continue
		}
		if line != first {
			t.Errorf("the build line differs between `lit %s` and `lit %s`, so the banner makes a command-specific claim:\n%s\n%s", firstName, cmd.name, first, line)
		}
	}
}

// buildLineOf returns the one `build:` line out of a command's output, failing
// the test when there is not exactly one — a caller asserting on "the build
// line" must not silently assert on the first of several, or on none.
func buildLineOf(t *testing.T, out string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "build: ") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one build line, got %d:\n%s", len(found), out)
	}
	return found[0]
}

// TestReadCommandsStaySilentOnAFreshOrReleasedBuild is acceptance criterion 2,
// and it is the half that protects the other half. The note's whole value is
// that it is rare: one that printed on every invocation would be tuned out
// within a session, and the loud case would go with it. A fresh source build and
// a release build are every ordinary invocation.
func TestReadCommandsStaySilentOnAFreshOrReleasedBuild(t *testing.T) {
	tenDaysAgo := time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339)

	cases := []struct {
		name   string
		origin string
		date   string
	}{
		{"fresh source build", version.OriginSource, time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)},
		// Dated well past the threshold on purpose: a release binary ages by
		// design and is refreshed by `lit upgrade`, not a rebuild, so age alone
		// must not make it speak.
		{"released build, ten days old", version.OriginRelease, tenDaysAgo},
		// The pre-ldflags `go build`: no trustworthy Date, so no age can be
		// claimed and none is.
		{"source build with no stamped date", version.OriginSource, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stampBuild(t, tc.origin, tc.date)
			dir, ticketID := unpushedCloneWithOneLocalChange(t)
			for _, cmd := range bannerReadCommands(ticketID) {
				out := runCLIInDir(t, dir, cmd.args...)
				if strings.Contains(out, "may predate fixes already on master") {
					t.Errorf("lit %s warned about the build for a %s:\n%s", cmd.name, tc.name, out)
				}
			}
		})
	}
}

// TestStaleBuildWarningLeadsTheBanner pins the position the ticket asks for.
// Build drift is the deepest of the three things this banner reports: a binary
// that predates master can be why the sync lines below it read as they do, so a
// reader who takes it in first interprets the rest correctly. Position is not
// cosmetic here — the incident this ticket comes from is a reader who saw the
// sync lines, found them unremarkable, and never learned about the binary.
func TestStaleBuildWarningLeadsTheBanner(t *testing.T) {
	stampBuild(t, version.OriginSource, time.Now().Add(-10*24*time.Hour).UTC().Format(time.RFC3339))
	dir, ticketID := unpushedCloneWithOneLocalChange(t)

	for _, cmd := range bannerReadCommands(ticketID) {
		out := runCLIInDir(t, dir, cmd.args...)
		build := strings.Index(out, "may predate fixes already on master")
		sync := strings.Index(out, "not pushed")
		if build < 0 {
			t.Fatalf("lit %s did not warn about the stale build:\n%s", cmd.name, out)
		}
		if sync < 0 {
			t.Fatalf("lit %s no longer produces the sync warning, so the ordering assertion proves nothing:\n%s", cmd.name, out)
		}
		if build > sync {
			t.Errorf("on lit %s the build warning trails the sync warning; build drift must lead:\n%s", cmd.name, out)
		}
	}
}
