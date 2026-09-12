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

// TestNextAndBacklogAnnounceAStaleBuild is acceptance criterion 1 of
// links-build-status-1svs: the two commands an agent runs every session — and
// `next` is the one that makes a routing DECISION — say so when the binary
// making that decision predates the tree it was built from. Before this, both
// printed their sync-staleness warning in exactly the right position and said
// nothing about the binary, and a session spent a full investigation inside
// routing code that was already correct.
func TestNextAndBacklogAnnounceAStaleBuild(t *testing.T) {
	stampBuild(t, version.OriginSource, time.Now().Add(-10*24*time.Hour).UTC().Format(time.RFC3339))
	// A workspace with a real ticket in it, so `next` reaches an actual routing
	// decision rather than an empty-backlog refusal — the decision is what the
	// ticket says must announce the binary behind it.
	dir, _ := unpushedCloneWithOneLocalChange(t)

	for _, cmd := range []string{"next", "backlog"} {
		out := runCLIInDir(t, dir, cmd)
		if !strings.Contains(out, "built 10 days ago") {
			t.Errorf("lit %s did not announce the stale build:\n%s", cmd, out)
		}
		if !strings.Contains(out, "just install") {
			t.Errorf("lit %s announced staleness without naming the remedy:\n%s", cmd, out)
		}
	}
}

// TestNextAndBacklogStaySilentOnAFreshOrReleasedBuild is acceptance criterion 2,
// and it is the half that protects the other half. The note's whole value is
// that it is rare: one that printed on every invocation would be tuned out
// within a session, and the loud case would go with it. A fresh source build and
// a release build are every ordinary invocation.
func TestNextAndBacklogStaySilentOnAFreshOrReleasedBuild(t *testing.T) {
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
			dir, _ := unpushedCloneWithOneLocalChange(t)
			for _, cmd := range []string{"next", "backlog"} {
				out := runCLIInDir(t, dir, cmd)
				if strings.Contains(out, "may predate fixes already on master") {
					t.Errorf("lit %s warned about the build for a %s:\n%s", cmd, tc.name, out)
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
	dir, _ := unpushedCloneWithOneLocalChange(t)

	out := runCLIInDir(t, dir, "backlog")
	build := strings.Index(out, "may predate fixes already on master")
	sync := strings.Index(out, "not pushed")
	if build < 0 {
		t.Fatalf("backlog did not warn about the stale build:\n%s", out)
	}
	if sync < 0 {
		t.Fatalf("fixture no longer produces the sync warning, so the ordering assertion proves nothing:\n%s", out)
	}
	if build > sync {
		t.Errorf("the build warning trails the sync warning; build drift must lead:\n%s", out)
	}
}
