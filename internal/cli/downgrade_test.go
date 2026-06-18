package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/release"
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
}

func (s *stubResolver) Resolve(_ context.Context, tag, _ string) (*release.Target, error) {
	s.called = true
	s.gotTag = tag
	if s.err != nil {
		return nil, s.err
	}
	return s.target, nil
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
	err          error
	called       bool
	gotTargetVer int64
}

func (s *stubDowngrader) Downgrade(_ context.Context, target int64) error {
	s.called = true
	s.gotTargetVer = target
	return s.err
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
	res := &stubResolver{target: newFakeTarget()}
	dg := &stubDowngrader{}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runDowngradeWith(context.Background(), &out, dg, []string{"--to", "v0.4.1"}, res, inst, fixedBinPath("/usr/local/bin/lit", nil))
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
	res := &stubResolver{target: newFakeTarget()}
	dg := &stubDowngrader{}
	inst := &stubInstaller{err: errors.New("network down")}
	var out bytes.Buffer
	err := runDowngradeWith(context.Background(), &out, dg, []string{"--to", "v0.4.1"}, res, inst, fixedBinPath("/p/lit", nil))
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
	res := &stubResolver{target: newFakeTarget()}
	dg := &stubDowngrader{err: errors.New("schema refused")}
	inst := &stubInstaller{}
	var out bytes.Buffer
	err := runDowngradeWith(context.Background(), &out, dg, []string{"--to", "v0.4.1"}, res, inst, fixedBinPath("/p/lit", nil))
	if err == nil || !strings.Contains(err.Error(), "schema refused") {
		t.Fatalf("expected schema error to propagate, got %v", err)
	}
	if inst.called {
		t.Error("installer must not run when schema downgrade fails")
	}
}

func TestNormalizeDowngradeTag(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{"v0.4.1", "v0.4.1", ""},
		{"0.4.1", "v0.4.1", ""},
		{" v0.4.1 ", "v0.4.1", ""},
		{"", "", "required"},
		{"v0.4.1/etc", "", "not a valid"},
		{"v0.4..1", "", "not a valid"},
		{"v0 .4.1", "", "not a valid"},
	}
	for _, c := range cases {
		got, err := normalizeDowngradeTag(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("normalizeDowngradeTag(%q) err = %v; want contains %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeDowngradeTag(%q) err = %v; want nil", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeDowngradeTag(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
