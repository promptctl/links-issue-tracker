package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/templates"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

func TestHooksInstallWritesPrePushHook(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(runEnv(t), repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	var stdout bytes.Buffer
	if err := runWsFamily(t, hooksFamily, context.Background(), &stdout, ws, []string{"install"}); err != nil {
		t.Fatalf("hooks install error = %v", err)
	}

	hookPath := filepath.Join(ws.GitCommonDir, "hooks", "pre-push")
	content, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("ReadFile(pre-push) error = %v", err)
	}
	text := string(content)
	if !strings.Contains(text, litHookMarkers.begin) || !strings.Contains(text, litHookMarkers.end) {
		t.Fatalf("hook missing managed section markers: %q", text)
	}
	if !strings.Contains(text, "hook-triggered lit sync push exited") {
		t.Fatalf("hook missing warning output: %q", text)
	}
	if !strings.Contains(text, "LNKS_AUTOMATION_TRIGGER=\"git-pre-push\"") {
		t.Fatalf("hook missing automation trigger env: %q", text)
	}
	if !strings.Contains(text, "LNKS_AUTOMATION_TRACE_REF_FILE") {
		t.Fatalf("hook missing trace ref env: %q", text)
	}
	if !strings.Contains(text, "exit 0") {
		t.Fatalf("hook must never block push: %q", text)
	}
}

// A push the hook triggers can fail in ways no retry clears (a peer's
// commits); only the failure's own block names the remedy. The
// hook must pass that block through and never claim the failure is transient.
func TestPrePushHookSurfacesTheFailuresOwnRemedy(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		// bash there may be WSL's, which sees neither the hook's path nor binDir.
		t.Skip("the hook runs under a POSIX bash")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	const remedy = "HOW TO RESOLVE: run `lit sync pull`, then `lit sync push`."
	cases := []struct {
		name string
		// existing is a user's pre-push hook the managed section is added to.
		existing   string
		stub       string
		wantStderr []string
	}{
		{
			name:       "failing push",
			stub:       "echo pushed-summary\necho '" + remedy + "' >&2\nexit 1\n",
			wantStderr: []string{remedy, "hook-triggered lit sync push exited 1 (trigger=git-pre-push remote=peer trace=unavailable)"},
		},
		{
			name:       "failing push that recorded a trace",
			stub:       "printf '/traces/push-1.json' > \"$LNKS_AUTOMATION_TRACE_REF_FILE\"\nexit 3\n",
			wantStderr: []string{"exited 3 (trigger=git-pre-push remote=peer trace=/traces/push-1.json)"},
		},
		{
			name:       "failing push after a user hook that sets errexit",
			existing:   "#!/usr/bin/env bash\nset -euo pipefail\n",
			stub:       "exit 5\n",
			wantStderr: []string{"exited 5 (trigger=git-pre-push remote=peer"},
		},
		{name: "succeeding push", stub: "echo pushed-summary\nexit 0\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			runGit(t, repo, "init")
			ws, err := workspace.Resolve(runEnv(t), repo)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			hookPath := filepath.Join(ws.GitCommonDir, "hooks", "pre-push")
			if tc.existing != "" {
				if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
					t.Fatalf("MkdirAll(hooks) error = %v", err)
				}
				if err := os.WriteFile(hookPath, []byte(tc.existing), 0o755); err != nil {
					t.Fatalf("WriteFile(existing pre-push) error = %v", err)
				}
			}
			if err := runWsFamily(t, hooksFamily, context.Background(), new(bytes.Buffer), ws, []string{"install"}); err != nil {
				t.Fatalf("hooks install error = %v", err)
			}
			binDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(binDir, "lit"), []byte("#!/bin/sh\n"+tc.stub), 0o755); err != nil {
				t.Fatalf("WriteFile(stub lit) error = %v", err)
			}
			cmd := exec.Command(bash, hookPath, "peer")
			cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			var hookStdout, hookStderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &hookStdout, &hookStderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("hook exited %v, want 0 (it must never block the push); stderr=%q", err, hookStderr.String())
			}
			got := hookStderr.String()
			for _, want := range tc.wantStderr {
				if !strings.Contains(got, want) {
					t.Errorf("hook stderr = %q, want it to contain %q", got, want)
				}
			}
			if len(tc.wantStderr) == 0 && got != "" {
				t.Errorf("hook stderr = %q, want silence on success", got)
			}
			for _, claim := range []string{"retry", "transient"} {
				if strings.Contains(strings.ToLower(got), claim) {
					t.Errorf("hook stderr = %q claims %q", got, claim)
				}
			}
			if hookStdout.Len() != 0 {
				t.Errorf("hook stdout = %q, want the push's summary discarded", hookStdout.String())
			}
		})
	}
}

func TestHooksInstallPreservesExistingPrePushHook(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(runEnv(t), repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	hooksDir := filepath.Join(ws.GitCommonDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(hooks) error = %v", err)
	}
	original := "#!/usr/bin/env bash\necho custom-pre-push\n"
	originalPath := filepath.Join(hooksDir, "pre-push")
	if err := os.WriteFile(originalPath, []byte(original), 0o755); err != nil {
		t.Fatalf("WriteFile(pre-push) error = %v", err)
	}

	if err := runHooksInstall(t, new(bytes.Buffer), ws, nil); err != nil {
		t.Fatalf("runHooksInstall() error = %v", err)
	}

	newHook, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatalf("ReadFile(new pre-push) error = %v", err)
	}
	newHookText := string(newHook)
	if !strings.Contains(newHookText, "echo custom-pre-push") {
		t.Fatalf("new hook does not preserve existing logic: %q", newHookText)
	}
	if !strings.Contains(newHookText, litHookMarkers.begin) || !strings.Contains(newHookText, litHookMarkers.end) {
		t.Fatalf("new hook missing links managed section: %q", newHookText)
	}
}

func TestHooksInstallMigratesLegacyMarkers(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runGit(t, repo, "init")
	ws, err := workspace.Resolve(runEnv(t), repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	hooksDir := filepath.Join(ws.GitCommonDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(hooks) error = %v", err)
	}
	hookPath := filepath.Join(hooksDir, "pre-push")
	seeded := "#!/usr/bin/env bash\necho user-prefix\n\n" +
		legacyHookMarkers.begin + "\necho stale-managed-content\n" + legacyHookMarkers.end + "\n"
	if err := os.WriteFile(hookPath, []byte(seeded), 0o755); err != nil {
		t.Fatalf("WriteFile(legacy pre-push) error = %v", err)
	}

	if err := runHooksInstall(t, new(bytes.Buffer), ws, nil); err != nil {
		t.Fatalf("runHooksInstall() error = %v", err)
	}

	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("ReadFile(pre-push) error = %v", err)
	}
	text := string(got)
	if strings.Contains(text, legacyHookMarkers.begin) || strings.Contains(text, legacyHookMarkers.end) {
		t.Fatalf("legacy markers not migrated: %q", text)
	}
	if strings.Count(text, litHookMarkers.begin) != 1 || strings.Count(text, litHookMarkers.end) != 1 {
		t.Fatalf("expected exactly one managed section, got: %q", text)
	}
	if !strings.Contains(text, "echo user-prefix") {
		t.Fatalf("user-owned prefix dropped: %q", text)
	}
}

func TestRunHooksViaCLI(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runGit(t, repo, "init")

	var stdout bytes.Buffer
	if err := Run(context.Background(), dirInvocation(t, repo, &stdout, &stdout, []string{"hooks", "install"})); err != nil {
		t.Fatalf("Run(hooks install) error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "installed") {
		t.Fatalf("hooks install output = %q, want it to report installed", output)
	}
	if !strings.Contains(output, filepath.Join("hooks", "pre-push")) {
		t.Fatalf("hooks install output = %q, want the pre-push hook path", output)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

// The pre-push-hook template carries the same hazard as the agents section: a
// marker-less override would replace the managed region with unmarked script on
// its first install and re-append the whole section on every install after,
// growing an executable hook without bound.
func TestHooksInstallMarkerlessOverrideConverges(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runGit(t, repo, "init")
	setRunEnv(t, "XDG_CONFIG_HOME", t.TempDir())
	writeProjectTemplateOverride(t, repo, templates.PrePushHookTemplateName, "echo house-hook\n")
	ws, err := workspace.Resolve(runEnv(t), repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	if _, err := installHooks(runEnv(t), ws); err != nil {
		t.Fatalf("installHooks() first run error = %v", err)
	}
	hookPath := filepath.Join(ws.GitCommonDir, "hooks", "pre-push")
	first := readFileString(t, hookPath)

	for run := 2; run <= 4; run++ {
		result, err := installHooks(runEnv(t), ws)
		if err != nil {
			t.Fatalf("installHooks() run %d error = %v", run, err)
		}
		if result.Changed {
			t.Fatalf("run %d reported a change on an already-converged hook", run)
		}
		if got := readFileString(t, hookPath); got != first {
			t.Fatalf("pre-push drifted on run %d:\ngot  %q\nwant %q", run, got, first)
		}
	}

	if n := strings.Count(first, "echo house-hook"); n != 1 {
		t.Fatalf("override body appears %d times, want 1: %q", n, first)
	}
	if strings.Count(first, litHookMarkers.begin) != 1 || strings.Count(first, litHookMarkers.end) != 1 {
		t.Fatalf("expected exactly one managed section, got: %q", first)
	}
}

// A malformed hook override is refused by name, and the hook on disk is left
// exactly as the user had it.
func TestHooksInstallRejectsUnbalancedOverride(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runGit(t, repo, "init")
	setRunEnv(t, "XDG_CONFIG_HOME", t.TempDir())
	writeProjectTemplateOverride(t, repo, templates.PrePushHookTemplateName,
		litHookMarkers.begin+"\necho house-hook\n"+litHookMarkers.end+"\necho trailer\n")
	ws, err := workspace.Resolve(runEnv(t), repo)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	hooksDir := filepath.Join(ws.GitCommonDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(hooks) error = %v", err)
	}
	hookPath := filepath.Join(hooksDir, "pre-push")
	seeded := "#!/usr/bin/env bash\necho user-prefix\n"
	if err := os.WriteFile(hookPath, []byte(seeded), 0o755); err != nil {
		t.Fatalf("WriteFile(pre-push) error = %v", err)
	}

	_, err = installHooks(runEnv(t), ws)
	if err == nil {
		t.Fatalf("installHooks() succeeded on an unbalanced override")
	}
	if got := ExitCode(err); got != ExitValidation {
		t.Fatalf("ExitCode() = %d, want %d (%v)", got, ExitValidation, err)
	}
	if !strings.Contains(err.Error(), "pre-push hook template (via project)") {
		t.Fatalf("error = %v, want it to name the template and its layer", err)
	}
	if got := readFileString(t, hookPath); got != seeded {
		t.Fatalf("pre-push rewritten despite the refusal: %q", got)
	}
}
