package cli

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/environ"
)

// TestGatesRefuseWhenTheStreamsAreTheNullDevice pins that the null device is no
// terminal, though it is a character device: `lit start >/dev/null` on a held
// lane and `lit purge >/dev/null` give the refusal naming the flag that
// confirms, never a prompt nobody can see answered by an EOF read as "no".
func TestGatesRefuseWhenTheStreamsAreTheNullDevice(t *testing.T) {
	t.Parallel()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = null.Close() })
	if interactive(null, null) {
		t.Fatalf("interactive(%s, %s) = true, want false", os.DevNull, os.DevNull)
	}

	_, err = confirmFreshTakeover(null, null, "lane held by another checkout", takesOver, false)
	var takeover takeoverUnconfirmedError
	if !errors.As(err, &takeover) || !strings.Contains(takeover.Message, "lane held by another checkout") || !strings.Contains(takeover.Message, "pass --take") {
		t.Errorf("confirmFreshTakeover over %s = %v, want the --take refusal naming the holder", os.DevNull, err)
	}

	err = confirmPurge(null, null, "links-abc", false)
	var purge purgeUnconfirmedError
	if !errors.As(err, &purge) || !strings.Contains(purge.Message, "pass --yes") {
		t.Errorf("confirmPurge over %s = %v, want the --yes refusal", os.DevNull, err)
	}

	// An editor launched here would fail to start and return an error.
	inv := Invocation{Dir: WorkingDirOf(t.TempDir(), nil), Env: environ.Of([]string{"EDITOR=/nonexistent/editor"}), Stdin: null, Stdout: null, Stderr: null}
	if err := openOrPrintWorkflowFile(inv, "point.md"); err != nil {
		t.Errorf("openOrPrintWorkflowFile over %s with $EDITOR set = %v, want the path printed and no editor", os.DevNull, err)
	}
}
