package cli

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/app"
	"github.com/promptctl/links-issue-tracker/internal/backup"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// Three CLI paths need two capabilities at once, and each one refuses an engine
// that offers only the first: `lit sync reconcile` (sync at the family
// boundary, reconcile per subcommand), the inline receive, and `restore` (sync
// and import, both resolved before anything is read, written, or backed up).
// Dolt offers all seven capabilities, so none of the three decline arms runs in
// any other test — they are correct by construction, which is exactly the state
// where a refactor turns a typed refusal into a nil dereference and nothing
// says so.
//
// What makes the engines below honest rather than the stub this ticket spent
// two rounds refusing: they are not Dolt with a capability hidden. They are the
// two partial shapes the CONTRACT already names. capabilities.go says the event
// store "offers sync and not reconcile: its arrival needs no merge ... so it
// has no diverged state, no base to merge through, and no side to take", and
// Importer says "an engine may be readable-out without being replaceable-in".
// A test engine standing for a shape the contract documents is an input value,
// not a fake of a particular engine. [LAW:behavior-not-structure]
//
// The construction is internal/storage/capabilities_test.go's, unchanged: an
// embedded nil interface promotes the whole method set, so a shape is declared
// in one line and a call to any method the engine never claimed panics instead
// of receiving a zero value. That panic is load-bearing here — it is what makes
// "nothing was read" observable without asserting which internal calls ran.
// [LAW:no-silent-failure]

// syncOnlyEngine offers sync and declines the other six. It is the event
// store's designed shape, and it stands at two of the three boundaries below:
// reconcile is a separate capability, and so is import.
type syncOnlyEngine struct {
	storage.Store
	storage.Syncer
}

// importOnlyEngine offers import and declines sync — an engine that can replace
// its own contents but has no remotes to compare them against. That is the
// memory engine's shape, and it is what reaches restore's FIRST resolve, where
// syncOnlyEngine reaches only the second.
type importOnlyEngine struct {
	storage.Store
	storage.Importer
}

// declineWorkspace is a workspace with nothing but somewhere to write traces
// and backups, which is all any of these paths touches before it refuses.
func declineWorkspace(t *testing.T) workspace.Info {
	t.Helper()
	return workspace.Info{Location: workspace.Location{StorageDir: t.TempDir()}}
}

// wantUnsupported asserts the error is the contract's typed refusal naming the
// capability that was missing, rather than some message that happens to mention
// it. Dispatching on the type is the whole reason UnsupportedError is a value
// and not a string. [LAW:parse-dont-validate]
func wantUnsupported(t *testing.T, err error, capability string) {
	t.Helper()
	var unsupported storage.UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v (%T), want a storage.UnsupportedError", err, err)
	}
	if unsupported.Capability != capability {
		t.Fatalf("refused capability %q, want %q", unsupported.Capability, capability)
	}
	if unsupported.Engine == "" {
		t.Fatal("refusal names no engine; a message that blames storage in the abstract cannot be acted on")
	}
}

// Every reconcile subcommand asks through reconcilerFor, and reconcilerFor
// traces the decline under the command that needed it — that is the claim its
// [LAW:single-enforcer] citation makes, and four handlers each recording the
// refusal their own way is the drift it exists to prevent. So the table is the
// four command names, and what it pins is that the decline reads identically
// under all of them except for which command asked.
func TestReconcilerForRefusesAnEngineThatDeclinesReconcile(t *testing.T) {
	t.Parallel()

	for _, command := range []string{
		reconcileShowCommand,
		proseResolveCommand,
		reconcileCombineCommand,
		"lit sync reconcile take " + string(storage.TakeLocal),
	} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			ws := declineWorkspace(t)

			reconciler, err := reconcilerFor(ws, syncSession{engine: syncOnlyEngine{}}, command)

			wantUnsupported(t, err, "reconcile")
			if reconciler != nil {
				t.Fatal("a refused reconcile handed back a non-nil Reconciler; the caller could hold and call it")
			}

			traces := readSyncTraces(t, ws)
			if len(traces) != 1 {
				t.Fatalf("recorded %d traces, want exactly 1: the decline is traced here so the handlers do not each trace it too", len(traces))
			}
			trace := traces[0]
			if trace.Command != command {
				t.Fatalf("trace command = %q, want %q: the decline must be recorded under the command that needed it", trace.Command, command)
			}
			if trace.Status != "error" || trace.Decision != "error" {
				t.Fatalf("trace status/decision = %q/%q, want error/error", trace.Status, trace.Decision)
			}
			if trace.Reason != err.Error() {
				t.Fatalf("trace reason = %q, want the refusal itself (%q)", trace.Reason, err.Error())
			}
		})
	}
}

// The inline receive reaches reconcile only after it has fast-forwarded
// everything it could and found a real divergence, so an engine that declines
// reconcile has nothing left to try. The danger is not that it fails — it is
// that the failure gets rounded into a settled outcome and the divergence is
// recorded as handled. This asserts the decline against the whole closed set of
// settled states, so a future state added to that set cannot quietly become a
// place a decline can land. [LAW:no-silent-failure]
func TestInlineReconcileSurfacesADeclineInsteadOfSettling(t *testing.T) {
	t.Parallel()
	ws := declineWorkspace(t)

	outcome := performInlineReconcile(context.Background(), syncSession{engine: syncOnlyEngine{}}, ws, "origin", "lit-sync")

	wantUnsupported(t, outcome.err, "reconcile")
	for _, settled := range []storage.SyncReconcileState{
		storage.SyncReconcileNotDiverged,
		storage.SyncReconcileLinearized,
		storage.SyncReconcileProsePending,
		storage.SyncReconcileUnrelated,
		storage.SyncReconcileTookLocal,
		storage.SyncReconcileTookRemote,
		storage.SyncReconcileCombined,
	} {
		if outcome.state == settled {
			t.Fatalf("a declined reconcile reported state %q; a divergence nobody resolved would read as one that was", settled)
		}
	}

	traces := readSyncTraces(t, ws)
	if len(traces) != 1 {
		t.Fatalf("recorded %d traces, want exactly 1", len(traces))
	}
	trace := traces[0]
	if trace.Status != "error" || trace.Decision != "error" {
		t.Fatalf("trace status/decision = %q/%q, want error/error: the durable trail is where an operator learns the divergence still stands",
			trace.Status, trace.Decision)
	}
	if trace.Reason != outcome.err.Error() || trace.Metadata["error"] != outcome.err.Error() {
		t.Fatalf("trace reason/metadata error = %q/%q, want the refusal (%q)", trace.Reason, trace.Metadata["error"], outcome.err.Error())
	}
	if reason := reconcileReasonForState(outcome.state); trace.Reason == reason {
		t.Fatalf("trace reason is the state reading %q rather than the failure; the decline would read as an outcome", reason)
	}
}

// Restore asks for sync and import before it reads, writes, or backs up, so
// that a half-capable engine cannot strand it half-done. Both halves of that
// claim are asserted, and the ordering half is asserted WITHOUT naming an
// internal call: the export path handed in does not exist, so a resolve that
// slipped below syncfile.Read would surface a missing-file error instead of the
// refusal, and a resolve below the export read would panic on the nil Store.
// [LAW:behavior-not-structure]
func TestRestoreRefusesAPartialEngineBeforeTouchingAnything(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		engine     storage.Store
		capability string
	}{
		{"an engine that cannot sync is refused at the first resolve", importOnlyEngine{}, "sync"},
		{"an engine that cannot import is refused at the second", syncOnlyEngine{}, "import"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ws := declineWorkspace(t)
			ap := &app.App{Workspace: ws, Store: tc.engine}

			err := restoreFromExportPath(context.Background(), ap, filepath.Join(t.TempDir(), "no-such-export.json"), false)

			wantUnsupported(t, err, tc.capability)
			snapshots, listErr := backup.List(ws.StorageDir)
			if listErr != nil {
				t.Fatalf("backup.List error = %v", listErr)
			}
			if len(snapshots) != 0 {
				t.Fatalf("a refused restore left %d backup(s); the capability question must be settled before anything is written", len(snapshots))
			}
		})
	}
}
