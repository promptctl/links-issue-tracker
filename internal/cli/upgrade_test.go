package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/release"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/telemetry"
	"github.com/promptctl/links-issue-tracker/internal/version"
)

// olderCurrentInfo is the running binary's identity in these tests: a release
// older than every fake target, so already-current never fires unless a test
// arranges the versions to match.
func olderCurrentInfo() version.Info {
	return version.Info{Version: "0.3.0", Schema: version.SchemaSupport{Min: 1, Max: 2}}
}

// appliedVersionFromOpenErr is the crux of the non-circular remediation: a
// schema-ahead refusal — the exact state `lit upgrade` is advertised for — must
// yield the workspace version so upgrade proceeds instead of dead-ending. Any
// other open failure must NOT be swallowed.
func TestAppliedVersionFromOpenErr(t *testing.T) {
	t.Parallel()
	// The schema-ahead refusal carries WorkspaceVersion as data; upgrade reads it.
	ahead := &store.UnsupportedSchemaVersionError{WorkspaceVersion: 7, MaxSupported: 3}
	if v, ok := appliedVersionFromOpenErr(ahead); !ok || v != 7 {
		t.Errorf("appliedVersionFromOpenErr(schema-ahead) = (%d, %v); want (7, true)", v, ok)
	}
	// Wrapped is still recovered (errors.As unwraps).
	if v, ok := appliedVersionFromOpenErr(fmt.Errorf("open: %w", ahead)); !ok || v != 7 {
		t.Errorf("appliedVersionFromOpenErr(wrapped) = (%d, %v); want (7, true)", v, ok)
	}
	// A genuine, unrelated open failure is NOT handled — it must propagate.
	if v, ok := appliedVersionFromOpenErr(errors.New("disk gone")); ok || v != 0 {
		t.Errorf("appliedVersionFromOpenErr(other) = (%d, %v); want (0, false)", v, ok)
	}
}

// stubSchemaReader returns a fixed applied schema version — no Dolt. The
// upgrade pipeline reads it once to decide the backward-move refusal.
type stubSchemaReader struct {
	version int64
	err     error
	called  bool
}

func (s *stubSchemaReader) AppliedSchemaVersion(_ context.Context, _ io.Writer) (int64, error) {
	s.called = true
	return s.version, s.err
}

// runUpgradeRecorded is runUpgradeWith under a recorder the test keeps, so it
// can read the facts the invocation's event carries.
func runUpgradeRecorded(
	t *testing.T,
	schema schemaReader,
	args []string,
	current version.Info,
	resolver upgradeResolver,
	installer release.Installer,
) (map[string]any, error) {
	t.Helper()
	recorder := telemetry.Begin(telemetry.Event{})
	ctx := telemetry.With(context.Background(), recorder)
	l := upgradeLeafWith(resolver, installer, fixedBinPath("/p/lit", nil))
	var out bytes.Buffer
	positional, err := parseLeaf(ctx, l, args, cwdInvocation(&out, &out, nil))
	if err != nil {
		t.Fatalf("parseLeaf: %v", err)
	}
	err = l.work(ctx, cwdInvocation(&out, &out, nil), upgradeScope{schema: schema, current: current}, positional)
	return writtenFacts(t, recorder.Finish(telemetry.End{})), err
}

// newFakeTarget (downgrade_test.go) reports Schema{Min:1, Max:3}. A workspace at
// v2 is BEHIND that target, so upgrading to it is the forward move upgrade owns.
func TestRunUpgradeWithHappyPath(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()}
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, olderCurrentInfo(), res, inst, fixedBinPath("/usr/local/bin/lit", nil))
	if err != nil {
		t.Fatalf("runUpgradeWith: %v", err)
	}
	if res.gotTag != "v0.9.0" {
		t.Errorf("resolver got tag %q; want v0.9.0", res.gotTag)
	}
	if !sr.called {
		t.Error("upgrade must read the workspace schema before installing")
	}
	if !inst.called || inst.gotTargetPath != "/usr/local/bin/lit" {
		t.Errorf("installer called=%v path=%q; want called=true path=/usr/local/bin/lit", inst.called, inst.gotTargetPath)
	}
	// The success line reads from → to, so the user sees what they left as
	// well as what they got.
	if !strings.Contains(out.String(), "upgraded v0.3.0 → v0.9.0") {
		t.Errorf("stdout missing from → to success line: %q", out.String())
	}
	// Upgrade never touches the schema — the target binary migrates forward on
	// its next Open. The success line must say so rather than claim a migration
	// this command performed.
	if !strings.Contains(out.String(), "next lit run migrates this workspace forward") {
		t.Errorf("stdout missing forward-migration handoff note: %q", out.String())
	}
}

// A target below the workspace, when this binary's schema support covers the
// span from the target up to the workspace, is a plain backward move: refuse
// before installing, and name lit downgrade (which can run here — this binary
// holds the down-migrations, so it reverses the schema, then installs the older
// binary).
func TestRunUpgradeWithTargetBehindWithinRunningSupportNamesDowngrade(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()} // Schema.Max == 3
	sr := &stubSchemaReader{version: 5}           // workspace ahead of the target
	current := version.Info{Version: "0.3.0", Schema: version.SchemaSupport{Min: 1, Max: 5}}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, current, res, inst, fixedBinPath("/p/lit", nil))
	var behind *UpgradeTargetBehindError
	if !errors.As(err, &behind) {
		t.Fatalf("err = %v (%T); want *UpgradeTargetBehindError", err, err)
	}
	if behind.Current != 5 || behind.Target != 3 || behind.Running != current.Schema {
		t.Errorf("err fields = {Current:%d Target:%d Running:%+v}; want {5 3 %+v}", behind.Current, behind.Target, behind.Running, current.Schema)
	}
	if !strings.Contains(behind.Error(), "lit downgrade --to v0.9.0") {
		t.Errorf("a backward move within this binary's schema support must name lit downgrade: %q", behind.Error())
	}
	if inst.called {
		t.Error("installer must not run when the target is behind the workspace")
	}
}

// A target below the workspace, when the workspace is past this binary's schema
// support, must NOT name lit downgrade — this binary does not carry the
// down-migrations a reverse needs. The remedy is a newer target.
func TestRunUpgradeWithTargetBehindPastRunningSupportNamesNewerTarget(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()} // Schema.Max == 3
	sr := &stubSchemaReader{version: 7}           // workspace past olderCurrentInfo's Schema.Max == 2
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, olderCurrentInfo(), res, inst, fixedBinPath("/p/lit", nil))
	var behind *UpgradeTargetBehindError
	if !errors.As(err, &behind) {
		t.Fatalf("err = %v (%T); want *UpgradeTargetBehindError", err, err)
	}
	if behind.Running != olderCurrentInfo().Schema {
		t.Errorf("Running = %+v; want %+v (this binary's schema support)", behind.Running, olderCurrentInfo().Schema)
	}
	msg := behind.Error()
	if strings.Contains(msg, "lit downgrade") {
		t.Errorf("a refusal past this binary's schema support must NOT suggest lit downgrade (it cannot run here): %q", msg)
	}
	if !strings.Contains(msg, "pick an upgrade target whose schema support reaches v7") || !strings.Contains(msg, "v1–v2, does not cover v3–v7") {
		t.Errorf("a refusal past this binary's schema support must point at a newer target: %q", msg)
	}
	if inst.called {
		t.Error("installer must not run when the target is behind the workspace")
	}
}

// The other way a reverse cannot run from here: the workspace is inside this
// binary's schema support, but the target sits below its baseline, and
// `lit downgrade` refuses a target below the baseline. The refusal must name a
// newer target, never `lit downgrade`.
func TestRunUpgradeWithTargetBelowRunningBaselineNamesNewerTarget(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()} // Schema.Max == 3
	sr := &stubSchemaReader{version: 5}           // workspace ahead of the target
	current := version.Info{Version: "0.3.0", Schema: version.SchemaSupport{Min: 4, Max: 9}}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, current, res, inst, fixedBinPath("/p/lit", nil))
	var behind *UpgradeTargetBehindError
	if !errors.As(err, &behind) {
		t.Fatalf("err = %v (%T); want *UpgradeTargetBehindError", err, err)
	}
	msg := behind.Error()
	if strings.Contains(msg, "lit downgrade") {
		t.Errorf("a target below this binary's baseline must NOT suggest lit downgrade (it refuses such a target): %q", msg)
	}
	if !strings.Contains(msg, "pick an upgrade target whose schema support reaches v5") || !strings.Contains(msg, "v4–v9, does not cover v3–v5") {
		t.Errorf("a target below this binary's baseline must point at a newer target: %q", msg)
	}
	if inst.called {
		t.Error("installer must not run when the target is behind the workspace")
	}
}

// A target whose schema support EQUALS the workspace's applied version is the
// local-ahead reinstall case (a newer binary wrote the workspace; the user is on
// an old one). Equality is not a backward move — install proceeds.
func TestRunUpgradeWithTargetEqualCurrentInstalls(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()} // Schema.Max == 3
	sr := &stubSchemaReader{version: 3}
	inst := &stubInstaller{}
	var out bytes.Buffer
	if err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, olderCurrentInfo(), res, inst, fixedBinPath("/p/lit", nil)); err != nil {
		t.Fatalf("runUpgradeWith at equal schema: %v", err)
	}
	if !inst.called {
		t.Error("installer must run when the target schema equals the workspace's")
	}
}

func TestRunUpgradeWithInstallFailureSurfacesRecovery(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()}
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{err: errors.New("network down")}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, olderCurrentInfo(), res, inst, fixedBinPath("/p/lit", nil))
	if err == nil {
		t.Fatal("expected install failure to surface as error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "installing v0.9.0 failed") {
		t.Errorf("recovery message missing install-failure framing: %q", msg)
	}
	if !strings.Contains(msg, "network down") {
		t.Errorf("recovery message should wrap underlying error: %q", msg)
	}
	// Upgrade never mutates the schema, so recovery must NOT mention a snapshot
	// restore (that is downgrade's post-schema-reverse recovery, not upgrade's).
	if strings.Contains(msg, "snapshots restore") {
		t.Errorf("upgrade recovery wrongly referenced snapshot restore: %q", msg)
	}
}

func TestRunUpgradeWithResolverErrorSkipsReadAndInstall(t *testing.T) {
	t.Parallel()
	res := &stubResolver{err: errors.New("manifest 404")}
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, olderCurrentInfo(), res, inst, fixedBinPath("/p/lit", nil))
	if err == nil || !strings.Contains(err.Error(), "manifest 404") {
		t.Fatalf("expected resolver error to propagate, got %v", err)
	}
	if sr.called {
		t.Error("schema read must not run when the manifest cannot be resolved")
	}
	if inst.called {
		t.Error("installer must not run when the manifest cannot be resolved")
	}
}

func TestRunUpgradeWithSchemaReadErrorSkipsInstall(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()}
	sr := &stubSchemaReader{err: errors.New("store closed")}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, olderCurrentInfo(), res, inst, fixedBinPath("/p/lit", nil))
	if err == nil || !strings.Contains(err.Error(), "read workspace schema version") {
		t.Fatalf("expected schema-read error to propagate, got %v", err)
	}
	if inst.called {
		t.Error("installer must not run when the schema read fails")
	}
}

// A --to that release-tags.sh does not call a release — v0.14.0.1 is a GitHub
// release with no assets — is refused as bad input before the resolver makes
// any request, so the user never sees a manifest download fail.
func TestRunUpgradeRefusesANonReleaseTagBeforeResolving(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()}
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.14.0.1"}, olderCurrentInfo(), res, inst, fixedBinPath("/p/lit", nil))
	if code := ExitCode(err); code != ExitValidation {
		t.Fatalf("exit = %d (err %v); want %d", code, err, ExitValidation)
	}
	if !strings.Contains(err.Error(), `"v0.14.0.1" is not a release tag`) {
		t.Errorf("err = %q; want it to name the tag as not a release", err)
	}
	if res.called || sr.called || inst.called {
		t.Errorf("a non-release --to reached resolve=%v read=%v install=%v; want all false", res.called, sr.called, inst.called)
	}
}

func TestRunUpgradeExtraArgsIsUsageError(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()}
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0", "extra"}, olderCurrentInfo(), res, inst, fixedBinPath("/p/lit", nil))
	var usage UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("err = %v (%T); want UsageError for an extra positional arg", err, err)
	}
	if !strings.Contains(usage.Error(), "usage: lit upgrade [--to <version>]") {
		t.Errorf("usage error text = %q; want the upgrade usage line", usage.Error())
	}
	// The NArg guard runs before resolve/read/install — none must fire.
	if res.called || sr.called || inst.called {
		t.Errorf("extra-arg guard leaked: resolve=%v read=%v install=%v; want all false", res.called, sr.called, inst.called)
	}
}

// Bare `lit upgrade` resolves its own target — the latest published release —
// and installs it. Nobody is sent hunting for a version tag, and the bare
// invocation lit's own sync-failure remediation prints runs as printed
// (links-upgrade-8ynx).
func TestRunUpgradeBareResolvesLatestAndInstalls(t *testing.T) {
	t.Parallel()
	res := &stubResolver{latestTag: mustTag("v0.9.0"), target: newFakeTarget()}
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{}, olderCurrentInfo(), res, inst, fixedBinPath("/p/lit", nil))
	if err != nil {
		t.Fatalf("bare runUpgradeWith: %v", err)
	}
	if !res.latestCalled {
		t.Error("bare invocation must resolve the latest release tag")
	}
	if res.gotTag != "v0.9.0" {
		t.Errorf("resolver got tag %q; want the latest-resolved v0.9.0", res.gotTag)
	}
	if !inst.called {
		t.Error("installer must run for a bare invocation that is not current")
	}
	if !strings.Contains(out.String(), "upgraded v0.3.0 → v0.9.0") {
		t.Errorf("stdout missing from → to success line: %q", out.String())
	}
}

// A bare invocation already on the latest release is a friendly no-op: exit 0,
// the kept version named, nothing installed.
func TestRunUpgradeBareAlreadyCurrentKeepsBinary(t *testing.T) {
	t.Parallel()
	res := &stubResolver{latestTag: mustTag("v0.4.1"), target: newFakeTarget()} // manifest Version 0.4.1
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	current := version.Info{Version: "0.4.1", Schema: version.SchemaSupport{Min: 1, Max: 3}}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{}, current, res, inst, fixedBinPath("/p/lit", nil))
	if err != nil {
		t.Fatalf("already-current must be a clean no-op, got %v", err)
	}
	if inst.called {
		t.Error("installer must not run when already on the latest release")
	}
	if !strings.Contains(out.String(), "already current") || !strings.Contains(out.String(), "v0.4.1") {
		t.Errorf("no-op line must say already current and name the kept version: %q", out.String())
	}
}

// The feed names the most recently CREATED release, not the highest version —
// a backport tag for an older line can be "latest" while the installed binary
// is newer. A bare invocation must keep the newer binary, never walk it
// backward silently; the no-op is an ordering check, not an equality check.
func TestRunUpgradeBareBinaryAheadOfLatestKeepsBinary(t *testing.T) {
	t.Parallel()
	tgt := newFakeTarget()
	tgt.Manifest.Version = "0.9.5"
	res := &stubResolver{latestTag: mustTag("v0.9.5"), target: tgt}
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	current := version.Info{Version: "0.10.0", Schema: version.SchemaSupport{Min: 1, Max: 5}}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{}, current, res, inst, fixedBinPath("/p/lit", nil))
	if err != nil {
		t.Fatalf("binary ahead of latest must be a clean no-op, got %v", err)
	}
	if inst.called {
		t.Error("installer must not run when the binary is ahead of the feed's latest")
	}
	if !strings.Contains(out.String(), "keeping v0.10.0") || !strings.Contains(out.String(), "v0.9.5") {
		t.Errorf("no-op line must name the kept version and the feed's latest: %q", out.String())
	}
}

// A pinned --to naming the running version still installs: the explicit tag is
// a command, and the reinstall path for a damaged binary. Only the unpinned
// default treats already-current as satisfied.
func TestRunUpgradePinnedSameVersionStillInstalls(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()} // manifest Version 0.4.1
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	current := version.Info{Version: "0.4.1", Schema: version.SchemaSupport{Min: 1, Max: 3}}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.4.1"}, current, res, inst, fixedBinPath("/p/lit", nil))
	if err != nil {
		t.Fatalf("pinned reinstall: %v", err)
	}
	if !inst.called {
		t.Error("installer must run for an explicit --to, even at the running version")
	}
}

// When the workspace schema is ahead of even the latest release, the bare
// invocation is refused loudly with both schema ranges named — never soothed
// with "already current", even if the running binary matches the latest
// release. The refusal outranks the no-op.
func TestRunUpgradeBareLatestBehindWorkspaceRefused(t *testing.T) {
	t.Parallel()
	res := &stubResolver{latestTag: mustTag("v0.4.1"), target: newFakeTarget()} // Schema.Max == 3
	sr := &stubSchemaReader{version: 7}                                         // workspace ahead of latest
	inst := &stubInstaller{}
	current := version.Info{Version: "0.4.1", Schema: version.SchemaSupport{Min: 1, Max: 3}}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{}, current, res, inst, fixedBinPath("/p/lit", nil))
	var behind *UpgradeTargetBehindError
	if !errors.As(err, &behind) {
		t.Fatalf("err = %v (%T); want *UpgradeTargetBehindError", err, err)
	}
	msg := behind.Error()
	if !strings.Contains(msg, "v3") || !strings.Contains(msg, "v7") {
		t.Errorf("refusal must name both the target's schema reach and the workspace's version: %q", msg)
	}
	if inst.called {
		t.Error("installer must not run when latest cannot cover the workspace schema")
	}
	if strings.Contains(out.String(), "already current") {
		t.Errorf("refusal must not be preceded by an already-current no-op: %q", out.String())
	}
}

// The whole bare-upgrade pipeline composed from the REAL release components —
// HTTPResolver (feed + manifest) and HTTPInstaller (download, checksum,
// atomic swap) — over a local httptest server: the closest a test can get to
// `lit upgrade` against the live feed without touching the network. The stub
// tests above pin each stage's contract; this one pins that the stages
// actually compose.
func TestRunUpgradeBareEndToEndOverHTTP(t *testing.T) {
	t.Parallel()
	newBinary := "NEW-BINARY-BYTES"
	var archive bytes.Buffer
	gw := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: "lit", Mode: 0o755, Size: int64(len(newBinary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(newBinary)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	sum := sha256.Sum256(archive.Bytes())

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	manifest := release.Manifest{
		Info: version.Info{Version: "0.9.9", Schema: version.SchemaSupport{Min: 1, Max: 5}},
		Artifacts: []release.Artifact{{
			Platform: release.CurrentPlatform(),
			URL:      srv.URL + "/dl/v0.9.9/lit.tar.gz",
			SHA256:   hex.EncodeToString(sum[:]),
		}},
	}
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v0.9.9","draft":false,"prerelease":false}`))
	})
	mux.HandleFunc("/dl/v0.9.9/release-manifest.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(&manifest)
	})
	mux.HandleFunc("/dl/v0.9.9/lit.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive.Bytes())
	})

	binPath := filepath.Join(t.TempDir(), "lit")
	if err := os.WriteFile(binPath, []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatalf("seed old binary: %v", err)
	}

	res := &release.HTTPResolver{BaseURL: srv.URL + "/dl", LatestURL: srv.URL + "/feed"}
	sr := &stubSchemaReader{version: 2}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{}, olderCurrentInfo(), res, &release.HTTPInstaller{}, fixedBinPath(binPath, nil))
	if err != nil {
		t.Fatalf("bare end-to-end upgrade: %v", err)
	}
	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != newBinary {
		t.Errorf("installed binary = %q; want the archive's payload", got)
	}
	if !strings.Contains(out.String(), "upgraded v0.3.0 → v0.9.9") {
		t.Errorf("stdout missing from → to line: %q", out.String())
	}
}

// An explicitly empty --to (a broken shell expansion: --to "$VERSION" with
// $VERSION unset) is a validation failure — never a silent upgrade to latest.
// Only a truly omitted flag means latest.
func TestRunUpgradeExplicitEmptyToIsRejected(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--to", ""}, {"--to", "   "}} {
		res := &stubResolver{latestTag: mustTag("v0.9.0"), target: newFakeTarget()}
		sr := &stubSchemaReader{version: 2}
		inst := &stubInstaller{}
		var out bytes.Buffer
		err := runUpgradeWith(context.Background(), &out, sr, args, olderCurrentInfo(), res, inst, fixedBinPath("/p/lit", nil))
		if err == nil || !strings.Contains(err.Error(), "non-empty version") {
			t.Fatalf("args %v: expected the non-empty-version validation error, got %v", args, err)
		}
		if res.latestCalled {
			t.Errorf("args %v: an explicit empty --to must not fall back to latest resolution", args)
		}
		if res.called || sr.called || inst.called {
			t.Errorf("args %v: empty --to leaked past validation: resolve=%v read=%v install=%v", args, res.called, sr.called, inst.called)
		}
	}
}

// A dev build is never told "already current" — the guard is typed on IsDev,
// so it holds even against a degenerate manifest whose Version is empty and
// would compare equal to the dev build's unstamped one.
func TestRunUpgradeBareDevBuildAlwaysInstalls(t *testing.T) {
	t.Parallel()
	tgt := newFakeTarget()
	tgt.Manifest.Version = "" // degenerate: equal to the dev build's Version
	res := &stubResolver{latestTag: mustTag("v0.9.0"), target: tgt}
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	current := version.Info{IsDev: true, Schema: version.SchemaSupport{Min: 1, Max: 5}}
	var out bytes.Buffer
	if err := runUpgradeWith(context.Background(), &out, sr, []string{}, current, res, inst, fixedBinPath("/p/lit", nil)); err != nil {
		t.Fatalf("bare dev-build upgrade: %v", err)
	}
	if !inst.called {
		t.Error("installer must run for a dev build; IsDev outranks any version equality")
	}
	if strings.Contains(out.String(), "already current") {
		t.Errorf("dev build wrongly told already current: %q", out.String())
	}
	if !strings.Contains(out.String(), "upgraded dev build → v0.9.0") {
		t.Errorf("stdout missing dev-build from → to line: %q", out.String())
	}
}

// A latest-lookup failure stops the pipeline before anything else runs.
func TestRunUpgradeLatestTagErrorStopsPipeline(t *testing.T) {
	t.Parallel()
	res := &stubResolver{latestErr: errors.New("feed unreachable")}
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{}, olderCurrentInfo(), res, inst, fixedBinPath("/p/lit", nil))
	if err == nil || !strings.Contains(err.Error(), "feed unreachable") {
		t.Fatalf("expected latest-lookup error to propagate, got %v", err)
	}
	if res.called || sr.called || inst.called {
		t.Errorf("latest-lookup failure leaked: resolve=%v read=%v install=%v; want all false", res.called, sr.called, inst.called)
	}
}

// The case the open-succeeded proxy got wrong: a newer lit migrated the
// workspace past this binary's registry, but the baseline shape is intact, so
// this binary opens it cleanly. A downgrade from here would need the newer
// lit's down-migrations, so the refusal must name a newer target, never `lit
// downgrade`. The workspace is real and read through the production reader, so
// this also pins that a clean open yields the ahead version. The invocation's
// event carries the schema versions the refusal was decided from.
func TestRunUpgradeOnAnOpenableWorkspacePastRunningSupportNamesNoDowngrade(t *testing.T) {
	_, ws := initBootstrapTestRepo(t)
	current, err := version.Get()
	if err != nil {
		t.Fatalf("version.Get() error = %v", err)
	}
	ahead := current.Schema.Max + 1
	ctx := context.Background()
	st, err := engine.Open(ctx, testEnv(), testStderr(t), engine.ReadWrite, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("engine.Open() error = %v", err)
	}
	raw, err := storage.TestSupport.Of(st)
	if err != nil {
		t.Fatalf("test support: %v", err)
	}
	if err := raw.ExecRawForTest(ctx, "INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)", ahead); err != nil {
		t.Fatalf("stamp ahead version: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close() error = %v", err)
	}
	// The precondition the case is about: the ahead workspace opens cleanly, so
	// the version below comes from a clean open, not from a schema-ahead refusal.
	ro, err := engine.Open(ctx, testEnv(), testStderr(t), engine.ReadOnly, ws.DatabasePath, ws.WorkspaceID)
	if err != nil {
		t.Fatalf("read-only open of the ahead workspace: %v; want a clean open", err)
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("read-only store.Close() error = %v", err)
	}

	res := &stubResolver{target: newFakeTarget()} // Schema.Max == 3
	inst := &stubInstaller{}
	facts, err := runUpgradeRecorded(t, workspaceSchemaReader{ws: ws, env: testEnv()}, []string{"--to", "v0.9.0"}, current, res, inst)

	var behind *UpgradeTargetBehindError
	if !errors.As(err, &behind) {
		t.Fatalf("err = %v (%T); want *UpgradeTargetBehindError", err, err)
	}
	if behind.Current != ahead || behind.Running != current.Schema {
		t.Errorf("err fields = {Current:%d Running:%+v}; want {%d %+v}", behind.Current, behind.Running, ahead, current.Schema)
	}
	if msg := behind.Error(); strings.Contains(msg, "lit downgrade") {
		t.Errorf("refusal names lit downgrade, which cannot reverse migrations this binary does not carry: %q", msg)
	}
	if inst.called {
		t.Error("installer must not run when the target is behind the workspace")
	}
	want := map[string]any{
		"upgrade.tag":                "v0.9.0",
		"upgrade.workspace_schema":   float64(ahead),
		"upgrade.target_schema_max":  float64(3),
		"upgrade.running_schema_min": float64(current.Schema.Min),
		"upgrade.running_schema_max": float64(current.Schema.Max),
		// The reader's open built nothing, found nothing to migrate, and
		// did not look for temp table files.
		"bootstrap":                         "none",
		"migration.snapshot":                "",
		"migration.snapshot_left_out":       nil,
		"migration.prune_error":             nil,
		"migration.schema_ahead":            float64(ahead),
		"migration.min_writer_schema":       float64(0),
		"migration.min_writer_schema_error": nil,
		"migration.read_only":               false,
		"temp_table_files.files":            nil,
		"temp_table_files.bytes":            nil,
		"temp_table_files.error":            nil,
	}
	if !reflect.DeepEqual(facts, want) {
		t.Errorf("facts = %v; want %v", facts, want)
	}
}

// Keeping the binary and installing one both exit 0, so the event records which
// one the invocation decided: true for a bare run already on the latest
// release, false for one that installs.
func TestRunUpgradeRecordsWhetherItWasAlreadyCurrent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		current version.Info
		want    bool
	}{
		{"on the latest release", version.Info{Version: "0.4.1", Schema: version.SchemaSupport{Min: 1, Max: 3}}, true},
		{"behind the latest release", olderCurrentInfo(), false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := &stubResolver{latestTag: mustTag("v0.4.1"), target: newFakeTarget()} // manifest Version 0.4.1
			inst := &stubInstaller{}
			facts, err := runUpgradeRecorded(t, &stubSchemaReader{version: 2}, []string{}, tc.current, res, inst)
			if err != nil {
				t.Fatalf("bare upgrade: %v", err)
			}
			if got := facts["upgrade.already_current"]; got != tc.want {
				t.Errorf("upgrade.already_current = %v; want %v", got, tc.want)
			}
			if inst.called == tc.want {
				t.Errorf("installer called = %v; want %v", inst.called, !tc.want)
			}
		})
	}
}

// An unpinned upgrade resolves the latest release itself, so the event records
// which release that was — the one it installed, or kept the binary past —
// and, only once the install succeeded, where it installed it.
func TestRunUpgradeRecordsTheReleaseItResolvedAndWhereItInstalled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		current       version.Info
		installErr    error
		wantInstalled any
	}{
		{"installs the latest release", olderCurrentInfo(), nil, "/p/lit"},
		{"keeps a binary already on it", version.Info{Version: "0.4.1", Schema: version.SchemaSupport{Min: 1, Max: 3}}, nil, nil},
		{"fails to install it", olderCurrentInfo(), errors.New("network down"), nil},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := &stubResolver{latestTag: mustTag("v0.4.1"), target: newFakeTarget()}
			facts, err := runUpgradeRecorded(t, &stubSchemaReader{version: 2}, []string{}, tc.current, res, &stubInstaller{err: tc.installErr})
			if (err != nil) != (tc.installErr != nil) {
				t.Fatalf("bare upgrade err = %v; want an error only when the install fails", err)
			}
			if got := facts["upgrade.tag"]; got != "v0.4.1" {
				t.Errorf("upgrade.tag = %v; want v0.4.1", got)
			}
			if got := facts["upgrade.installed_path"]; got != tc.wantInstalled {
				t.Errorf("upgrade.installed_path = %v; want %v", got, tc.wantInstalled)
			}
		})
	}
}

// A release that does not resolve still leaves the event the tag the command
// asked for — the latest one it looked up, for a bare run — the one fact known
// before Resolve.
func TestRunUpgradeRecordsTheTagOfAReleaseThatDoesNotResolve(t *testing.T) {
	t.Parallel()
	res := &stubResolver{latestTag: mustTag("v0.4.1"), err: errors.New("release v0.4.1 not found")}
	facts, err := runUpgradeRecorded(t, &stubSchemaReader{version: 2}, []string{}, olderCurrentInfo(), res, &stubInstaller{})
	if err == nil {
		t.Fatal("an unresolvable release must fail the upgrade")
	}
	if want := map[string]any{"upgrade.tag": "v0.4.1"}; !reflect.DeepEqual(facts, want) {
		t.Errorf("facts = %v; want %v", facts, want)
	}
}

// A workspace read that fails still leaves the event the schema versions known
// before it: the target's and this binary's, and no workspace version.
func TestRunUpgradeSchemaReadErrorKeepsTheVersionsKnownBeforeIt(t *testing.T) {
	t.Parallel()
	res := &stubResolver{target: newFakeTarget()} // Schema.Max == 3
	inst := &stubInstaller{}
	facts, err := runUpgradeRecorded(t, &stubSchemaReader{err: errors.New("dolt locked")}, []string{"--to", "v0.9.0"}, olderCurrentInfo(), res, inst)
	if err == nil {
		t.Fatal("a failed workspace read must fail the upgrade")
	}
	want := map[string]any{
		"upgrade.tag":                "v0.9.0",
		"upgrade.target_schema_max":  float64(3),
		"upgrade.running_schema_min": float64(1),
		"upgrade.running_schema_max": float64(2),
	}
	if !reflect.DeepEqual(facts, want) {
		t.Errorf("facts = %v; want %v", facts, want)
	}
}
