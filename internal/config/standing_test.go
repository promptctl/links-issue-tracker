package config

import (
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func TestStandingComparesTheFileWithTheConfigInForce(t *testing.T) {
	const a, b = "[ready]\nrequired_fields = [\"prompt\"]\n", "[quickstart]\nsoil_mode = true\n"
	file := func(v string) FileVersion { return FileVersion{Present: true, Content: v} }
	none := FileVersion{}
	applied := []model.ConfigApply{{ID: "cfg-1", Content: a, CreatedAt: time.Unix(1, 0)}}
	for _, tc := range []struct {
		name               string
		working, committed FileVersion
		applies            []model.ConfigApply
		want               Standing
	}{
		{"no file, nothing applied: the defaults", none, none, nil, StandingInForce},
		{"the committed file is the one applied", file(a), file(a), applied, StandingInForce},
		{"a committed file nothing applied", file(a), file(a), nil, StandingNotApplied},
		{"a committed file other than the one applied", file(b), file(b), applied, StandingNotApplied},
		{"an edit not committed", file(b), file(a), applied, StandingUncommitted},
		{"a file HEAD lacks", file(a), none, applied, StandingUncommitted},
		{"a deletion not committed", none, file(a), applied, StandingUncommitted},
		{"no file, a config applied", none, none, applied, StandingNoFile},
	} {
		got := StandingOf(ProjectFile{Path: "p", Working: tc.working, Committed: tc.committed}, tc.applies)
		if got != tc.want {
			t.Errorf("%s: standing = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestApplyRefusesAFileForEveryFaultItHolds(t *testing.T) {
	const invalid = "[sync]\ncadence = \"hourly\"\n[snapshot]\nretention_budget = 0\n"
	for _, tc := range []struct {
		name   string
		file   ProjectFile
		faults []string
	}{
		{"no file", ProjectFile{Path: "p"}, []string{"no project config file at p", "p is not committed"}},
		{"not committed and invalid", ProjectFile{Path: "p", Working: FileVersion{Present: true, Content: invalid}},
			[]string{"p is not committed", "sync.cadence", "snapshot.retention_budget"}},
		{"uncommitted changes", ProjectFile{Path: "p", Working: FileVersion{Present: true, Content: "x = 1\n"}, Committed: FileVersion{Present: true, Content: ""}},
			[]string{"p has uncommitted changes"}},
	} {
		_, err := tc.file.Applicable()
		if err == nil {
			t.Errorf("%s: applied, want refused", tc.name)
			continue
		}
		for _, fault := range tc.faults {
			if !strings.Contains(err.Error(), fault) {
				t.Errorf("%s: refusal %q does not name %q", tc.name, err, fault)
			}
		}
	}
	content, err := ProjectFile{Path: "p", Working: FileVersion{Present: true, Content: "[quickstart]\nsoil_mode = true\n"}, Committed: FileVersion{Present: true, Content: "[quickstart]\nsoil_mode = true\n"}}.Applicable()
	if err != nil || content != "[quickstart]\nsoil_mode = true\n" {
		t.Errorf("a committed valid file = (%q, %v), want its content", content, err)
	}
}

func TestLoadNamesEveryFaultTheConfigHolds(t *testing.T) {
	_, err := Load(testEnv(), appliedProject("[sync]\ncadence = \"hourly\"\n[snapshot]\nretention_budget = 0\n[claims]\nfreshness_window = \"soon\"\n"))
	if err == nil {
		t.Fatal("loaded, want refused")
	}
	for _, fault := range []string{"sync.cadence", "snapshot.retention_budget", "freshness_window"} {
		if !strings.Contains(err.Error(), fault) {
			t.Errorf("refusal %q does not name %q", err, fault)
		}
	}
}

func TestTheAppliedLayerIsTheConfigInForce(t *testing.T) {
	if got := Applied(nil).Source(); got != "none" {
		t.Errorf("nothing applied: source = %q, want none", got)
	}
	applies := []model.ConfigApply{
		{ID: "cfg-1", Content: "[quickstart]\nsoil_mode = false\n", CreatedAt: time.Unix(1, 0)},
		{ID: "cfg-2", Content: "[quickstart]\nsoil_mode = true\n", CreatedAt: time.Unix(2, 0)},
	}
	layer := Applied(applies)
	if want := "applied:" + string(applies[1].Digest()); layer.Source() != want {
		t.Errorf("source = %q, want %q", layer.Source(), want)
	}
	cfg, err := Load(testEnv(), layer)
	if err != nil || !cfg.Quickstart.SoilMode {
		t.Errorf("loaded (%+v, %v), want the newest apply's soil_mode", cfg.Quickstart, err)
	}
}
