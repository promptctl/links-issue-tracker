package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// newNestedEpicsInDir files epic outer and epic inner under it in the
// workspace at dir and returns their ids.
func newNestedEpicsInDir(t *testing.T, dir string) (outer, inner string) {
	t.Helper()
	outer = newEpicInDir(t, dir, "Outer")
	inner = extractTicketID(t, runCLIInDir(t, dir, "new", "--type", "epic", "--title", "Inner", "--topic", "demo", "--parent", outer))
	return outer, inner
}

// runCLIInDirWithin is runCLIInDirAllowError under a deadline, so a command
// that never returns fails the test that ran it instead of hanging the suite.
func runCLIInDirWithin(t *testing.T, dir string, limit time.Duration, args ...string) (string, error) {
	t.Helper()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%q) error = %v", dir, err)
	}
	defer func() { _ = os.Chdir(prevWD) }()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	var out bytes.Buffer
	runErr := Run(ctx, cwdInvocation(&out, &out, args))
	return out.String(), runErr
}

// A backup holding a loop of epics restores: the landing cuts the edge the
// member with the greater id holds onto the other, prints the cut, records it
// on the restore's trace, and the store lists afterwards. Which edge goes is
// worked by hand from that rule.
func TestBackupRestoreCutsALoopTheBackupHolds(t *testing.T) {
	repo := newInitTestRepo(t)
	outer, inner := newNestedEpicsInDir(t, repo)
	created := strings.Fields(runCLIInDir(t, repo, "backup", "create"))
	if len(created) != 2 {
		t.Fatalf("backup create printed %v, want a name and a path", created)
	}
	payload, err := os.ReadFile(created[1])
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	var backup map[string]any
	if err := json.Unmarshal(payload, &backup); err != nil {
		t.Fatalf("decode backup: %v", err)
	}
	relations, _ := backup["relations"].([]any)
	if len(relations) != 1 {
		t.Fatalf("backup holds %d relation(s), want the one edge %s -> %s", len(relations), inner, outer)
	}
	closing := map[string]any{}
	for key, value := range relations[0].(map[string]any) {
		closing[key] = value
	}
	closing["src_id"], closing["dst_id"] = outer, inner
	backup["relations"] = append(relations, closing)
	edited, err := json.Marshal(backup)
	if err != nil {
		t.Fatalf("encode backup: %v", err)
	}
	path := filepath.Join(t.TempDir(), "looped.json")
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	lo, hi := min(outer, inner), max(outer, inner)

	out, err := runCLIInDirWithin(t, repo, 60*time.Second, "backup", "restore", "--path", path, "--force")
	if err != nil {
		t.Fatalf("restore of a looped backup error = %v\noutput:\n%s", err, out)
	}
	for _, want := range []string{
		"cut 1 parent edge(s) the backup held in a loop",
		quoteRemote(hi).inline() + " no longer under " + quoteRemote(lo).inline(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("restore output %q lacks %q", out, want)
		}
	}
	restore := restoreTraceRecord(t, repo)
	if want := hi + " no longer under " + lo + " (loop " + lo + " -> " + hi + ")"; restore.Metadata["unlooped"] != "1" || restore.Metadata["unlooped_cuts"] != want {
		t.Errorf("restore trace metadata = %v, want unlooped=1 and the cut %q", restore.Metadata, want)
	}
	listed, err := runCLIInDirWithin(t, repo, 60*time.Second, "ls")
	if err != nil {
		t.Fatalf("ls after the restore error = %v\noutput:\n%s", err, listed)
	}
	for _, id := range []string{outer, inner} {
		if !strings.Contains(listed, id) {
			t.Errorf("ls after the restore = %q, want it to list %s", listed, id)
		}
	}
}

// A store that already holds a loop of epics — written before the rules that
// refuse and cut one — answers `lit ls` with a refusal naming the loop and the
// repair, as corruption, rather than descending into the loop until killed.
func TestListRefusesAStoredLoopByName(t *testing.T) {
	repo := newInitTestRepo(t)
	outer, inner := newNestedEpicsInDir(t, repo)
	ctx := context.Background()
	ap, err := app.Open(ctx, testEnv(), resolvedWorkspace(t, repo), app.AccessWrite)
	if err != nil {
		t.Fatalf("app.Open() error = %v", err)
	}
	raw, err := storage.TestSupport.Of(ap.Store)
	if err != nil {
		t.Fatalf("TestSupport: %v", err)
	}
	if err := raw.ExecRawForTest(ctx,
		`INSERT INTO relations(src_id, dst_id, type, created_at, created_by) VALUES (?, ?, 'parent-child', ?, 'import')`,
		outer, inner, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("plant parent loop: %v", err)
	}
	if err := ap.Close(); err != nil {
		t.Fatalf("close app: %v", err)
	}

	out, err := runCLIInDirWithin(t, repo, 60*time.Second, "ls")
	if err == nil {
		t.Fatalf("ls over a loop succeeded:\n%s", out)
	}
	if got := ExitCode(err); got != ExitCorruption {
		t.Errorf("exit code = %d, want %d (corruption); error = %v", got, ExitCorruption, err)
	}
	if got := commandErrorReason(err); got != reasonParentCycle {
		t.Errorf("reason = %q, want %q; error = %v", got, reasonParentCycle, err)
	}
	for _, want := range []string{outer, inner, "lit parent set --child"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}
