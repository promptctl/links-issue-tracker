package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func chdirTempRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	runGit(t, repo, "init")
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })
	return repo
}

func runLit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := Run(context.Background(), &stdout, &stdout, args)
	return stdout.String(), err
}

func TestQuickstartBareOutputIsRouter(t *testing.T) {
	chdirTempRepo(t)

	output, err := runLit(t, "quickstart")
	if err != nil {
		t.Fatalf("Run(quickstart) error = %v", err)
	}
	for _, topic := range quickstartTopicTokens() {
		if !strings.Contains(output, "lit quickstart "+topic) {
			t.Fatalf("router output missing topic %q:\n%s", topic, output)
		}
	}
	for _, fastpath := range []string{"`lit ready`", "`lit start <id>`"} {
		if !strings.Contains(output, fastpath) {
			t.Fatalf("router output missing fastpath line %q:\n%s", fastpath, output)
		}
	}
	if !strings.Contains(output, "cheap to call") {
		t.Fatalf("router output missing re-run encouragement line:\n%s", output)
	}
}

func TestQuickstartTopicPrintsTaskGuidance(t *testing.T) {
	chdirTempRepo(t)

	// Each topic's guidance must mention the command the topic is about.
	wantByTopic := map[string]string{
		"ready":  "lit ready",
		"new":    "lit new",
		"update": "lit rank",
		"done":   "lit done",
		"doctor": "lit doctor",
	}
	for topic, want := range wantByTopic {
		output, err := runLit(t, "quickstart", topic)
		if err != nil {
			t.Fatalf("Run(quickstart %s) error = %v", topic, err)
		}
		if !strings.Contains(output, want) {
			t.Fatalf("quickstart %s output missing %q:\n%s", topic, want, output)
		}
		if strings.Contains(output, "lit quickstart ready` —") {
			t.Fatalf("quickstart %s printed the router instead of topic guidance:\n%s", topic, output)
		}
	}
}

func TestQuickstartUnknownTopicErrors(t *testing.T) {
	chdirTempRepo(t)

	_, err := runLit(t, "quickstart", "bogus")
	if err == nil {
		t.Fatal("Run(quickstart bogus) expected error, got nil")
	}
	if !strings.Contains(err.Error(), "ready, new, update, done, doctor") {
		t.Fatalf("unknown-topic error should list valid topics, got %q", err.Error())
	}
}

func TestQuickstartTopicRejectsFlags(t *testing.T) {
	chdirTempRepo(t)

	for _, args := range [][]string{
		{"quickstart", "ready", "--refresh"},
		{"quickstart", "ready", "--eject"},
	} {
		if _, err := runLit(t, args...); err == nil {
			t.Fatalf("Run(%v) expected error, got nil", args)
		}
	}
}

func TestQuickstartSoilSectionOnRouterOnly(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	configDir := filepath.Join(xdg, "links-issue-tracker")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(config dir) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[quickstart]\nsoil_mode = true\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(config.toml) error = %v", err)
	}
	repo := t.TempDir()
	runGit(t, repo, "init")
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("Chdir(repo) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })

	router, err := runLit(t, "quickstart")
	if err != nil {
		t.Fatalf("Run(quickstart) error = %v", err)
	}
	if !strings.Contains(router, "## Soil") {
		t.Fatalf("router output with soil_mode=true missing Soil section:\n%s", router)
	}
	topic, err := runLit(t, "quickstart", "ready")
	if err != nil {
		t.Fatalf("Run(quickstart ready) error = %v", err)
	}
	if strings.Contains(topic, "## Soil") {
		t.Fatalf("topic output must not carry the Soil section:\n%s", topic)
	}
}

func TestQuickstartTopicHonorsProjectOverride(t *testing.T) {
	repo := chdirTempRepo(t)

	overridePath := filepath.Join(repo, ".lit", "templates", "quickstart-ready.md")
	if err := os.MkdirAll(filepath.Dir(overridePath), 0o755); err != nil {
		t.Fatalf("MkdirAll(project templates) error = %v", err)
	}
	if err := os.WriteFile(overridePath, []byte("## Custom ready guidance\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(override) error = %v", err)
	}

	output, err := runLit(t, "quickstart", "ready")
	if err != nil {
		t.Fatalf("Run(quickstart ready) error = %v", err)
	}
	if !strings.Contains(output, "## Custom ready guidance") {
		t.Fatalf("topic output should honor the project override:\n%s", output)
	}
}
