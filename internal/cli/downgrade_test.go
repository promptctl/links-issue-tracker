package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/release"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// stubResolver returns a fixed Target — no HTTP. The CLI pipeline reads
// target.Manifest.Schema.Max and passes target to the installer; the rest
// of the manifest can be empty for these tests.
type stubResolver struct {
	target *release.Target
	err    error
	called bool
	gotTag string
	// latest-lookup side, exercised only by upgrade's bare invocation;
	// downgrade never calls LatestTag (its dependency is Resolver alone).
	latestTag    release.Tag
	latestErr    error
	latestCalled bool
}

// mustTag parses a release tag a test names as a literal.
func mustTag(s string) release.Tag {
	tag, err := release.ParseTag(s)
	if err != nil {
		panic(err)
	}
	return tag
}

func (s *stubResolver) Resolve(_ context.Context, tag release.Tag, _ string) (*release.Target, error) {
	s.called = true
	s.gotTag = tag.String()
	if s.err != nil {
		return nil, s.err
	}
	return s.target, nil
}

func (s *stubResolver) LatestTag(_ context.Context) (release.Tag, error) {
	s.latestCalled = true
	if s.latestErr != nil {
		return release.Tag{}, s.latestErr
	}
	return s.latestTag, nil
}

type stubInstaller struct {
	err           error
	called        bool
	gotTarget     *release.Target
	gotTargetPath string
}

func (s *stubInstaller) Install(_ context.Context, t *release.Target, path string) error {
	s.called = true
	s.gotTarget = t
	s.gotTargetPath = path
	return s.err
}

type stubDowngrader struct {
	done         *storage.Downgraded
	err          error
	called       bool
	gotTargetVer int64
}

func (s *stubDowngrader) Downgrade(_ context.Context, target int64) (*storage.Downgraded, error) {
	s.called = true
	s.gotTargetVer = target
	return s.done, s.err
}

// reversedFromV4 is the store's report of a reverse from v4 to newFakeTarget's
// v3: one down-migration, after one recovery snapshot.
func reversedFromV4() *storage.Downgraded {
	return &storage.Downgraded{From: 4, Reversed: 1, Snapshot: &storage.RecoverySnapshot{Name: "1700000000000000000-lit-downgrade-1700000000000000001", LeftOut: []string{"links/.dolt/git-remote-cache"}}}
}

// newerCurrentInfo is a running binary whose schema support covers the span
// down to newFakeTarget's v3 from a workspace at v4.
func newerCurrentInfo() version.Info {
	return version.Info{Version: "0.5.0", Schema: version.SchemaSupport{Min: 1, Max: 4}}
}

// runDowngradeRecorded is downgrade's parse and work under a recorder the test
// keeps, ended the way the dispatcher ends it, so it can read the facts and
// counts the invocation's event carries — those an error records included —
// and what it printed.
func runDowngradeRecorded(t *testing.T, scope downgradeScope, res release.Resolver, inst release.Installer, binPath func() (string, error)) (map[string]any, map[string]int, string, error) {
	t.Helper()
	recorder := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(context.Background(), recorder)
	l := downgradeLeafWith(res, inst, binPath)
	var out bytes.Buffer
	positional, err := parseLeaf(ctx, l, []string{"--to", "v0.4.1"}, &out)
	if err != nil {
		t.Fatalf("parseLeaf: %v", err)
	}
	err = l.work(ctx, &out, scope, positional)
	ev := recorder.Finish(invocationEnd(time.Now(), err, nil))
	return writtenFacts(t, ev), ev.Counts, out.String(), err
}

func fixedBinPath(path string, err error) func() (string, error) {
	return func() (string, error) { return path, err }
}

func newFakeTarget() *release.Target {
	return &release.Target{
		Manifest: release.Manifest{
			Info:      version.Info{Version: "0.4.1", Schema: version.SchemaSupport{Min: 1, Max: 3}},
			Artifacts: []release.Artifact{{Platform: release.CurrentPlatform(), URL: "https://example/x.tar.gz", SHA256: strings.Repeat("0", 64)}},
		},
		Artifact: release.Artifact{
			Platform: release.CurrentPlatform(),
			URL:      "https://example/x.tar.gz",
			SHA256:   strings.Repeat("0", 64),
		},
	}
}

func TestRunDowngradeWithHappyPath(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()}
	dg := &stubDowngrader{done: reversedFromV4()}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runDowngradeWith(context.Background(), &out, downgradeScope{schema: dg, current: newerCurrentInfo()}, []string{"--to", "v0.4.1"}, res, inst, fixedBinPath("/usr/local/bin/lit", nil))
	if err != nil {
		t.Fatalf("runDowngradeWith: %v", err)
	}
	if res.gotTag != "v0.4.1" {
		t.Errorf("resolver got tag %q; want v0.4.1", res.gotTag)
	}
	if !dg.called || dg.gotTargetVer != 3 {
		t.Errorf("downgrader called=%v target=%d; want called=true target=3", dg.called, dg.gotTargetVer)
	}
	if !inst.called || inst.gotTargetPath != "/usr/local/bin/lit" {
		t.Errorf("installer called=%v path=%q; want called=true path=/usr/local/bin/lit", inst.called, inst.gotTargetPath)
	}
	if !strings.Contains(out.String(), "downgraded to v0.4.1") {
		t.Errorf("stdout missing success line: %q", out.String())
	}
}

func TestRunDowngradeWithInstallFailureSurfacesRecovery(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()}
	dg := &stubDowngrader{done: reversedFromV4()}
	inst := &stubInstaller{err: errors.New("network down")}
	var out bytes.Buffer
	err := runDowngradeWith(context.Background(), &out, downgradeScope{schema: dg, current: newerCurrentInfo()}, []string{"--to", "v0.4.1"}, res, inst, fixedBinPath("/p/lit", nil))
	if err == nil {
		t.Fatal("expected install failure to surface as error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "schema reversed to v3") {
		t.Errorf("recovery message missing schema reference: %q", msg)
	}
	if !strings.Contains(msg, "lit snapshots restore") {
		t.Errorf("recovery message missing snapshot-restore instruction: %q", msg)
	}
	if !strings.Contains(msg, "network down") {
		t.Errorf("recovery message should wrap underlying error: %q", msg)
	}
}

func TestRunDowngradeWithSchemaErrorSkipsInstall(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()}
	dg := &stubDowngrader{err: errors.New("schema refused")}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runDowngradeWith(context.Background(), &out, downgradeScope{schema: dg, current: newerCurrentInfo()}, []string{"--to", "v0.4.1"}, res, inst, fixedBinPath("/p/lit", nil))
	if err == nil || !strings.Contains(err.Error(), "schema refused") {
		t.Fatalf("expected schema error to propagate, got %v", err)
	}
	if inst.called {
		t.Error("installer must not run when schema downgrade fails")
	}
}

// A downgrade that runs records what it did: the release it installed and
// where, the schema versions it was decided from — the workspace's as the
// store read it under the lock — how many down-migrations it reversed, and the
// snapshot it took first.
func TestRunDowngradeRecordsWhatItReversedAndInstalled(t *testing.T) {
	t.Parallel()
	facts, counts, _, err := runDowngradeRecorded(t, downgradeScope{schema: &stubDowngrader{done: reversedFromV4()}, current: newerCurrentInfo()}, &stubResolver{target: newFakeTarget()}, &stubInstaller{}, fixedBinPath("/p/lit", nil))
	if err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	want := map[string]any{
		"downgrade.tag":                  "v0.4.1",
		"downgrade.installed_path":       "/p/lit",
		"downgrade.workspace_schema":     float64(4),
		"downgrade.snapshot":             "1700000000000000000-lit-downgrade-1700000000000000001",
		"downgrade.snapshot_left_out":    []any{"links/.dolt/git-remote-cache"},
		"downgrade.snapshot_prune_error": nil,
		"downgrade.target_schema_max":    float64(3),
		"downgrade.running_schema_min":   float64(1),
		"downgrade.running_schema_max":   float64(4),
	}
	if !reflect.DeepEqual(facts, want) {
		t.Errorf("facts = %v; want %v", facts, want)
	}
	if want := map[string]int{"downgrade.reversed": 1}; !reflect.DeepEqual(counts, want) {
		t.Errorf("counts = %v; want %v", counts, want)
	}
}

// Whatever the reverse reports lands on the event, beside the facts set before
// it, whether or not the command then fails; and only an install that
// succeeded records where it installed. A target the workspace is already at
// says it reversed nothing with a zero, not by leaving the count out; a reverse
// that failed after committing says how far it got; a store that failed before
// reading the workspace's version records none of the reverse's facts.
func TestRunDowngradeRecordsWhatTheReverseReportedOnEveryOutcome(t *testing.T) {
	t.Parallel()
	afterTheReverse := errors.New("commit downgrade revert of v4: connection reset")
	reversed := map[string]any{
		"downgrade.workspace_schema":     float64(4),
		"downgrade.snapshot":             "1700000000000000000-lit-downgrade-1700000000000000001",
		"downgrade.snapshot_left_out":    []any{"links/.dolt/git-remote-cache"},
		"downgrade.snapshot_prune_error": nil,
	}
	tests := []struct {
		name       string
		dg         *stubDowngrader
		inst       *stubInstaller
		wantFacts  map[string]any
		wantCounts map[string]int
	}{
		{
			name: "already at the target",
			dg:   &stubDowngrader{done: &storage.Downgraded{From: 3}},
			inst: &stubInstaller{},
			wantFacts: map[string]any{
				"downgrade.workspace_schema":     float64(3),
				"downgrade.snapshot":             "",
				"downgrade.snapshot_left_out":    nil,
				"downgrade.snapshot_prune_error": nil,
				"downgrade.installed_path":       "/p/lit",
			},
			wantCounts: map[string]int{"downgrade.reversed": 0},
		},
		{
			name:       "reverse fails after committing",
			dg:         &stubDowngrader{done: reversedFromV4(), err: afterTheReverse},
			inst:       &stubInstaller{},
			wantFacts:  reversed,
			wantCounts: map[string]int{"downgrade.reversed": 1},
		},
		{
			name:       "install fails",
			dg:         &stubDowngrader{done: reversedFromV4()},
			inst:       &stubInstaller{err: errors.New("network down")},
			wantFacts:  reversed,
			wantCounts: map[string]int{"downgrade.reversed": 1},
		},
		{
			name:       "store fails before reading the workspace",
			dg:         &stubDowngrader{err: errors.New("open workspace: locked")},
			inst:       &stubInstaller{},
			wantFacts:  map[string]any{},
			wantCounts: map[string]int{},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			facts, counts, _, _ := runDowngradeRecorded(t, downgradeScope{schema: tc.dg, current: newerCurrentInfo()}, &stubResolver{target: newFakeTarget()}, tc.inst, fixedBinPath("/p/lit", nil))
			want := map[string]any{
				"downgrade.tag":                "v0.4.1",
				"downgrade.target_schema_max":  float64(3),
				"downgrade.running_schema_min": float64(1),
				"downgrade.running_schema_max": float64(4),
			}
			maps.Copy(want, tc.wantFacts)
			if !reflect.DeepEqual(facts, want) {
				t.Errorf("facts = %v; want %v", facts, want)
			}
			if !reflect.DeepEqual(counts, tc.wantCounts) {
				t.Errorf("counts = %v; want %v", counts, tc.wantCounts)
			}
		})
	}
}

// A release that does not resolve still leaves the event the tag the command
// asked for, the one fact known before Resolve.
func TestRunDowngradeRecordsTheTagOfAReleaseThatDoesNotResolve(t *testing.T) {
	t.Parallel()
	res := &stubResolver{err: errors.New("release v0.4.1 not found")}
	facts, _, _, err := runDowngradeRecorded(t, downgradeScope{schema: &stubDowngrader{}, current: newerCurrentInfo()}, res, &stubInstaller{}, fixedBinPath("/p/lit", nil))
	if err == nil {
		t.Fatal("an unresolvable release must fail the downgrade")
	}
	if want := map[string]any{"downgrade.tag": "v0.4.1"}; !reflect.DeepEqual(facts, want) {
		t.Errorf("facts = %v; want %v", facts, want)
	}
}

// A newer lit migrated the workspace past this binary's support, leaving the
// baseline intact, so this binary opens it but does not carry the
// down-migrations a reverse would run. The workspace is real and the reverse is
// the production migrator's, so this pins the whole path: the refusal is the
// schema-ahead class — exit 3 and advice that leaves installing a different lit
// to the user, never validation_refused's "adjust the command", which no --to
// can satisfy — nothing is installed, and the event carries the span and the
// support the refusal was decided from.
func TestRunDowngradeOnAWorkspacePastRunningSupportIsSchemaAhead(t *testing.T) {
	_, ws := initBootstrapTestRepo(t)
	current, err := version.Get()
	if err != nil {
		t.Fatalf("version.Get() error = %v", err)
	}
	ahead := current.Schema.Max + 1
	ctx := context.Background()
	st, err := engine.Open(ctx, engine.ReadWrite, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	raw, err := storage.TestSupport.Of(st)
	if err != nil {
		t.Fatalf("test support: %v", err)
	}
	if err := raw.ExecRawForTest(ctx, "INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)", ahead); err != nil {
		t.Fatalf("stamp ahead version: %v", err)
	}
	migrator, err := storage.SchemaMigration.Of(st)
	if err != nil {
		t.Fatalf("schema migration: %v", err)
	}

	inst := &stubInstaller{}
	facts, counts, _, err := runDowngradeRecorded(t, downgradeScope{schema: migrator, current: current}, &stubResolver{target: newFakeTarget()}, inst, fixedBinPath("/p/lit", nil))
	if err == nil {
		t.Fatal("a downgrade past this binary's support must be refused")
	}
	if reason := commandErrorReason(err); reason != reasonWorkspaceSchemaAhead {
		t.Errorf("reason = %q; want %q (error: %v)", reason, reasonWorkspaceSchemaAhead, err)
	}
	if code := ExitCode(err); code != ExitValidation {
		t.Errorf("exit = %d; want %d", code, ExitValidation)
	}
	if inst.called {
		t.Error("installer must not run when the reverse is refused")
	}
	want := map[string]any{
		"downgrade.tag":                  "v0.4.1",
		"downgrade.workspace_schema":     float64(ahead),
		"downgrade.snapshot":             "",
		"downgrade.snapshot_left_out":    nil,
		"downgrade.snapshot_prune_error": nil,
		"downgrade.target_schema_max":    float64(3),
		"downgrade.running_schema_min":   float64(current.Schema.Min),
		"downgrade.running_schema_max":   float64(current.Schema.Max),
	}
	if !reflect.DeepEqual(facts, want) {
		t.Errorf("facts = %v; want %v", facts, want)
	}
	if want := map[string]int{"downgrade.reversed": 0}; !reflect.DeepEqual(counts, want) {
		t.Errorf("counts = %v; want %v", counts, want)
	}
}

// A downgrade whose reverse committed installs the release it reversed for
// even when rolling old downgrade snapshots off then failed: otherwise the
// newer binary stays installed and its next open migrates the workspace
// forward again. Nothing is printed before the install, and the prune failure
// ends whichever output ends the command, success or failure.
func TestRunDowngradeInstallsWhenTheSnapshotPruneFails(t *testing.T) {
	t.Parallel()
	pruneErr := errors.New("prune downgrade snapshots: permission denied")
	warning := "\nwarning: old downgrade snapshots were not removed: " + pruneErr.Error()
	notInstalled := "downgrade: schema reversed to v3 but installing prior binary failed: "
	tests := []struct {
		name          string
		binPath       func() (string, error)
		installErr    error
		wantInstalled bool
		wantOut       string
		wantErrPrefix string // "" when the command succeeds
		wantPath      any
	}{
		{
			name:          "install succeeds",
			binPath:       fixedBinPath("/p/lit", nil),
			wantInstalled: true,
			wantOut:       "downgraded to v0.4.1 (schema v3) installed at /p/lit\nre-run `lit version` to confirm." + warning + "\n",
			wantPath:      "/p/lit",
		},
		{
			name:          "install fails",
			binPath:       fixedBinPath("/p/lit", nil),
			installErr:    errors.New("rename: read-only file system"),
			wantInstalled: true,
			wantErrPrefix: notInstalled + "rename: read-only file system\n\nrecover by either:",
		},
		{
			name:          "binary path fails",
			binPath:       fixedBinPath("", errors.New("executable: not found")),
			wantErrPrefix: notInstalled + "resolve current binary: executable: not found\n\nrecover by either:",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			done := reversedFromV4()
			done.PruneErr = pruneErr
			inst := &stubInstaller{err: tc.installErr}
			facts, _, out, err := runDowngradeRecorded(t, downgradeScope{schema: &stubDowngrader{done: done}, current: newerCurrentInfo()}, &stubResolver{target: newFakeTarget()}, inst, tc.binPath)
			if inst.called != tc.wantInstalled {
				t.Errorf("installer called = %v; want %v", inst.called, tc.wantInstalled)
			}
			if out != tc.wantOut {
				t.Errorf("stdout = %q; want %q", out, tc.wantOut)
			}
			switch {
			case tc.wantErrPrefix == "" && err != nil:
				t.Errorf("err = %v; want nil", err)
			case tc.wantErrPrefix != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.wantErrPrefix) || !strings.HasSuffix(err.Error(), warning)):
				t.Errorf("err = %v; want it to start with %q and end with %q", err, tc.wantErrPrefix, warning)
			}
			if got := facts["downgrade.snapshot_prune_error"]; got != pruneErr.Error() {
				t.Errorf("downgrade.snapshot_prune_error = %v; want %q", got, pruneErr.Error())
			}
			if got := facts["downgrade.installed_path"]; got != tc.wantPath {
				t.Errorf("downgrade.installed_path = %v; want %v", got, tc.wantPath)
			}
		})
	}
}

// The mirror of TestRunUpgradeRefusesANonReleaseTagBeforeResolving: downgrade
// parses --to through the same normalizeReleaseTag, before any request.
func TestRunDowngradeRefusesANonReleaseTagBeforeResolving(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runDowngradeWith(context.Background(), &out, downgradeScope{}, []string{"--to", "v0.14.0.1"}, res, inst, fixedBinPath("/p/lit", nil))
	if code := ExitCode(err); code != ExitValidation {
		t.Fatalf("exit = %d (err %v); want %d", code, err, ExitValidation)
	}
	if res.called || inst.called {
		t.Errorf("a non-release --to reached resolve=%v install=%v; want both false", res.called, inst.called)
	}
}

func TestNormalizeReleaseTag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{"v0.4.1", "v0.4.1", ""},
		{"0.4.1", "v0.4.1", ""},
		{" v0.4.1 ", "v0.4.1", ""},
		{"", "", "non-empty version"},
		{"   ", "", "non-empty version"}, // whitespace-only trims to empty, same class as missing
		{"v0.4.1/etc", "", "is not a release tag"},
		{"v0.4..1", "", "is not a release tag"},
		{"v0 .4.1", "", "is not a release tag"},
		{"v0.14.0.1", "", "is not a release tag"}, // a published GitHub release with no assets
		{"0.4.1-rc1", "", "is not a release tag"},
	}
	// Exercise BOTH verbs: the accept/reject shape is verb-independent, but verb
	// colours the error text, so a regression in either direction's message must
	// be caught. [LAW:one-source-of-truth] one grammar, two error surfaces.
	for _, verb := range []string{"downgrade", "upgrade"} {
		for _, c := range cases {
			got, err := normalizeReleaseTag(c.in, verb)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("normalizeReleaseTag(%q, %q) err = %v; want contains %q", c.in, verb, err, c.wantErr)
				}
				// The verb prefixes every error, so a rename of the command's
				// error framing surfaces here rather than silently.
				if err != nil && !strings.HasPrefix(err.Error(), verb+":") {
					t.Errorf("normalizeReleaseTag(%q, %q) err = %q; want %q-prefixed", c.in, verb, err, verb)
				}
				// A refused tag is quoted as the user gave it, not as normalized,
				// so the message names input the user can find in their command.
				if c.wantErr == "is not a release tag" && err != nil && !strings.Contains(err.Error(), fmt.Sprintf("--to %q", c.in)) {
					t.Errorf("normalizeReleaseTag(%q, %q) err = %q; want it to quote the input as given", c.in, verb, err)
				}
				// Both flavors of bad --to input are the same failure class and
				// must exit alike: model.ValidationError (exit 3), never a plain error
				// (exit 1). [LAW:one-type-per-behavior]
				var ve model.ValidationError
				if !errors.As(err, &ve) {
					t.Errorf("normalizeReleaseTag(%q, %q) err = %v (%T); want model.ValidationError", c.in, verb, err, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("normalizeReleaseTag(%q, %q) err = %v; want nil", c.in, verb, err)
				continue
			}
			if got.String() != c.want {
				t.Errorf("normalizeReleaseTag(%q, %q) = %q; want %q", c.in, verb, got, c.want)
			}
		}
	}
}
