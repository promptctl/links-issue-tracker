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

// stubSchemaReader returns a fixed applied schema version — no Dolt. The upgrade
// pipeline reads it once to decide the backward-move refusal.
type stubSchemaReader struct {
	version int64
	err     error
	called  bool
}

func (s *stubSchemaReader) AppliedSchemaVersion(_ context.Context) (int64, error) {
	s.called = true
	return s.version, s.err
}

// newFakeTarget (downgrade_test.go) reports Schema{Min:1, Max:3}. A workspace at
// v2 is BEHIND that target, so upgrading to it is the forward move upgrade owns.
func TestRunUpgradeWithHappyPath(t *testing.T) {
	res := &stubResolver{target: newFakeTarget()}
	sr := &stubSchemaReader{version: 2}
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

// A target whose schema support ends BELOW the workspace's applied version is a
// backward move: refuse before installing, and name lit downgrade.
func TestRunUpgradeWithTargetBehindSkipsInstall(t *testing.T) {
	res := &stubResolver{target: newFakeTarget()} // Schema.Max == 3
	sr := &stubSchemaReader{version: 5}           // workspace ahead of the target
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{"--to", "v0.9.0"}, res, inst, fixedBinPath("/p/lit", nil))
	var behind *UpgradeTargetBehindError
	if !errors.As(err, &behind) {
		t.Fatalf("err = %v (%T); want *UpgradeTargetBehindError", err, err)
	}
	if behind.Current != 5 || behind.Target != 3 {
		t.Errorf("err fields = {Current:%d Target:%d}; want {5 3}", behind.Current, behind.Target)
	}
	if !strings.Contains(behind.Error(), "lit downgrade --to v0.9.0") {
		t.Errorf("backward-move error must name lit downgrade: %q", behind.Error())
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
	sr := &stubSchemaReader{version: 3}
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
	sr := &stubSchemaReader{version: 2}
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

func TestRunUpgradeMissingTagIsRequired(t *testing.T) {
	res := &stubResolver{target: newFakeTarget()}
	sr := &stubSchemaReader{version: 2}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runUpgradeWith(context.Background(), &out, sr, []string{}, res, inst, fixedBinPath("/p/lit", nil))
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected missing --to to be rejected as required, got %v", err)
	}
	if res.called || inst.called {
		t.Error("no resolve/install should run when --to is missing")
	}
}
