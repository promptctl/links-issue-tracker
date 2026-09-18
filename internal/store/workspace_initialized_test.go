package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUninitializedWorkspaceIsOneCondition drives every store entry point that
// can meet a repository `lit init` has never run in, and pins that each reports
// it as the same typed condition with the same sentence.
//
// That is the contract the CLI's reason and exit-code sinks read: before
// links-cli-errors-yfbg each site raised a bare fmt.Errorf, so the sinks could
// not recognize the condition and fell through to the unclassified-fault
// default, which told the agent to retry a state no retry can change. The
// entry points are driven for real rather than asserted about, so reverting any
// one of them to a fmt.Errorf carrying the identical text fails here.
// [LAW:behavior-not-structure]
func TestUninitializedWorkspaceIsOneCondition(t *testing.T) {
	t.Parallel()

	entryPoints := []struct {
		name string
		run  func(ctx context.Context, doltRoot string) error
	}{
		{"OpenForRead", func(ctx context.Context, doltRoot string) error {
			st, err := OpenForRead(ctx, doltRoot, "ws")
			if st != nil {
				_ = st.Close()
			}
			return err
		}},
		{"DumpRaw", func(ctx context.Context, doltRoot string) error {
			_, err := DumpRaw(ctx, doltRoot, "ws")
			return err
		}},
		{"LockDoltJournalExclusive", func(ctx context.Context, doltRoot string) error {
			release, err := LockDoltJournalExclusive(ctx, doltRoot)
			if err == nil {
				_ = release()
			}
			return err
		}},
	}

	for _, tc := range entryPoints {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doltRoot := filepath.Join(t.TempDir(), "dolt")

			err := tc.run(context.Background(), doltRoot)
			if err == nil {
				t.Fatalf("%s() on an uninitialized workspace succeeded; want the not-initialized refusal", tc.name)
			}
			if !errors.Is(err, ErrWorkspaceNotInitialized) {
				t.Fatalf("%s() error = %v; want it to report ErrWorkspaceNotInitialized so the CLI sinks can classify it", tc.name, err)
			}
			// One home means one sentence: an agent meeting this condition
			// through any entry point reads the same line, and no site can
			// re-spell it. [LAW:one-source-of-truth]
			if err.Error() != ErrWorkspaceNotInitialized.Error() {
				t.Fatalf("%s() error text = %q; want the single-home text %q", tc.name, err.Error(), ErrWorkspaceNotInitialized.Error())
			}
		})
	}
}

// TestRequireInitializedDirSeparatesGenuineStatFaults pins acceptance 4 of
// links-cli-errors-yfbg from below: only ENOENT means "never initialized". A
// stat that fails any other way is a fault the operator has to see, it names
// the directory that failed, and it must not be laundered into the tidy
// "run lit init" answer — which would send someone to initialize a workspace
// over a permissions or path fault that init cannot fix. [LAW:no-silent-failure]
func TestRequireInitializedDirSeparatesGenuineStatFaults(t *testing.T) {
	t.Parallel()

	notADir := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed the non-directory: %v", err)
	}

	err := requireInitializedDir(filepath.Join(notADir, "dolt"), "database dir")
	if err == nil {
		t.Fatal("requireInitializedDir() through a non-directory succeeded; want the stat fault surfaced")
	}
	if errors.Is(err, ErrWorkspaceNotInitialized) {
		t.Fatalf("a non-ENOENT stat fault was reported as an uninitialized workspace: %v", err)
	}
	if !strings.Contains(err.Error(), "stat database dir:") {
		t.Fatalf("stat fault = %v; want it to name the directory that failed", err)
	}
}
