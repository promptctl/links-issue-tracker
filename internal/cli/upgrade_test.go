package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

// appliedVersionFromOpenErr is the crux of the non-circular remediation: a
// schema-ahead refusal — the exact state `lit upgrade` is advertised for — must
// yield the workspace version so upgrade proceeds instead of dead-ending. Any
// other open failure must NOT be swallowed.
func TestAppliedVersionFromOpenErr(t *testing.T) {
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

// stubSchemaReader returns a fixed workspace schema — no Dolt. The upgrade
// pipeline reads it once to decide the backward-move refusal and which
// remediation the refusal names.
type stubSchemaReader struct {
	version  int64
	openable bool
	err      error
	called   bool
}

func (s *stubSchemaReader) ReadWorkspaceSchema(_ context.Context) (workspaceSchema, error) {
	s.called = true
	if s.err != nil {
		return workspaceSchema{}, s.err
	}
	return workspaceSchema{AppliedVersion: s.version, Openable: s.openable}, nil
}

// newFakeTarget (downgrade_test.go) reports Schema{Min:1, Max:3}. A workspace at
// v2 is BEHIND that target, so upgrading to it is the forward move upgrade owns.
func TestRunUpgradeWithHappyPath(t *testing.T) {
	res := &stubResolver{target: newFakeTarget()}
	sr := &stubSchemaReader{version: 2, openable: true}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, res, inst, fixedBinPath("/usr/local/bin/lit", nil))
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
	if !strings.Contains(out.String(), "upgraded to v0.9.0") {
		t.Errorf("stdout missing success line: %q", out.String())
	}
	// Upgrade never touches the schema — the target binary migrates forward on
	// its next Open. The success line must say so rather than claim a migration
	// this command performed.
	if !strings.Contains(out.String(), "next lit run migrates this workspace forward") {
		t.Errorf("stdout missing forward-migration handoff note: %q", out.String())
	}
}

// A target below the workspace, when this binary CAN open the workspace, is a
// plain backward move: refuse before installing, and name lit downgrade (which
// can run here — it reverses the schema, then installs the older binary).
func TestRunUpgradeWithTargetBehindOpenableNamesDowngrade(t *testing.T) {
	res := &stubResolver{target: newFakeTarget()}       // Schema.Max == 3
	sr := &stubSchemaReader{version: 5, openable: true} // workspace ahead of the target, but openable
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, res, inst, fixedBinPath("/p/lit", nil))
	var behind *UpgradeTargetBehindError
	if !errors.As(err, &behind) {
		t.Fatalf("err = %v (%T); want *UpgradeTargetBehindError", err, err)
	}
	if behind.Current != 5 || behind.Target != 3 || !behind.WorkspaceOpenable {
		t.Errorf("err fields = {Current:%d Target:%d Openable:%v}; want {5 3 true}", behind.Current, behind.Target, behind.WorkspaceOpenable)
	}
	if !strings.Contains(behind.Error(), "lit downgrade --to v0.9.0") {
		t.Errorf("openable backward-move error must name lit downgrade: %q", behind.Error())
	}
	if inst.called {
		t.Error("installer must not run when the target is behind the workspace")
	}
}

// A target below the workspace, when this binary CANNOT open the workspace (the
// version was recovered from a schema-ahead refusal), must NOT name lit downgrade
// — downgrade would hit the same refusal on open. The remedy is a newer target.
// This is the misleading-remediation trap the PR set out to kill, at the seam
// between the two features.
func TestRunUpgradeWithTargetBehindNotOpenableNamesNewerTarget(t *testing.T) {
	res := &stubResolver{target: newFakeTarget()}        // Schema.Max == 3
	sr := &stubSchemaReader{version: 7, openable: false} // workspace ahead; this binary can't open it
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, res, inst, fixedBinPath("/p/lit", nil))
	var behind *UpgradeTargetBehindError
	if !errors.As(err, &behind) {
		t.Fatalf("err = %v (%T); want *UpgradeTargetBehindError", err, err)
	}
	if behind.WorkspaceOpenable {
		t.Errorf("WorkspaceOpenable = true; want false (version came from the schema-ahead refusal)")
	}
	msg := behind.Error()
	if strings.Contains(msg, "lit downgrade") {
		t.Errorf("not-openable refusal must NOT suggest lit downgrade (dead here): %q", msg)
	}
	if !strings.Contains(msg, "pick an upgrade target") || !strings.Contains(msg, "cannot open") {
		t.Errorf("not-openable refusal must point at a newer target: %q", msg)
	}
	if inst.called {
		t.Error("installer must not run when the target is behind the workspace")
	}
}

// A target whose schema support EQUALS the workspace's applied version is the
// local-ahead reinstall case (a newer binary wrote the workspace; the user is on
// an old one). Equality is not a backward move — install proceeds.
func TestRunUpgradeWithTargetEqualCurrentInstalls(t *testing.T) {
	res := &stubResolver{target: newFakeTarget()} // Schema.Max == 3
	sr := &stubSchemaReader{version: 3, openable: true}
	inst := &stubInstaller{}
	var out bytes.Buffer
	if err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, res, inst, fixedBinPath("/p/lit", nil)); err != nil {
		t.Fatalf("runUpgradeWith at equal schema: %v", err)
	}
	if !inst.called {
		t.Error("installer must run when the target schema equals the workspace's")
	}
}

func TestRunUpgradeWithInstallFailureSurfacesRecovery(t *testing.T) {
	res := &stubResolver{target: newFakeTarget()}
	sr := &stubSchemaReader{version: 2, openable: true}
	inst := &stubInstaller{err: errors.New("network down")}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, res, inst, fixedBinPath("/p/lit", nil))
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
	res := &stubResolver{err: errors.New("manifest 404")}
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, res, inst, fixedBinPath("/p/lit", nil))
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
	res := &stubResolver{target: newFakeTarget()}
	sr := &stubSchemaReader{err: errors.New("store closed")}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, res, inst, fixedBinPath("/p/lit", nil))
	if err == nil || !strings.Contains(err.Error(), "read workspace schema version") {
		t.Fatalf("expected schema-read error to propagate, got %v", err)
	}
	if inst.called {
		t.Error("installer must not run when the schema read fails")
	}
}

func TestRunUpgradeExtraArgsIsUsageError(t *testing.T) {
	res := &stubResolver{target: newFakeTarget()}
	sr := &stubSchemaReader{version: 2, openable: true}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0", "extra"}, res, inst, fixedBinPath("/p/lit", nil))
	var usage UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("err = %v (%T); want UsageError for an extra positional arg", err, err)
	}
	if !strings.Contains(usage.Error(), "usage: lit upgrade --to <version>") {
		t.Errorf("usage error text = %q; want the upgrade usage line", usage.Error())
	}
	// The NArg guard runs before resolve/read/install — none must fire.
	if res.called || sr.called || inst.called {
		t.Errorf("extra-arg guard leaked: resolve=%v read=%v install=%v; want all false", res.called, sr.called, inst.called)
	}
}

func TestRunUpgradeMissingTagIsRequired(t *testing.T) {
	res := &stubResolver{target: newFakeTarget()}
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{}, res, inst, fixedBinPath("/p/lit", nil))
	if err == nil || !strings.Contains(err.Error(), "non-empty version") {
		t.Fatalf("expected missing --to to be rejected as a non-empty-version error, got %v", err)
	}
	// The tag check precedes resolve AND the schema read, so none must fire.
	if res.called || sr.called || inst.called {
		t.Errorf("missing-tag guard leaked: resolve=%v read=%v install=%v; want all false", res.called, sr.called, inst.called)
	}
}
