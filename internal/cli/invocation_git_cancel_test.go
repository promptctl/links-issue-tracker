package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/dolthub/dolt/go/libraries/utils/gitcmd"

	"github.com/promptctl/links-issue-tracker/internal/telemetry"
)

// A git run on the context dispatch runs under, and ended by that context, is
// one git.cancels occurrence on the invocation's event, whoever started it.
func TestInvocationRecordsACancelledGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn // held open; it never answers
		}
	}()
	url := "git://127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) + "/wedge.git"

	recorder := telemetry.Begin(telemetry.Event{})
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := invocationContext(parent, recorder)
	done := make(chan error, 1)
	go func() {
		_, err := gitcmd.Command(ctx, gitcmd.Spec{}, "ls-remote", url).Output()
		done <- err
	}()
	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("git never connected to the listener")
	}
	cancel()
	select {
	case err := <-done:
		var cancelled *gitcmd.Cancelled
		if !errors.As(err, &cancelled) {
			t.Fatalf("err = %v, want a *gitcmd.Cancelled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled git did not return within 5s")
	}

	ev := recorder.Finish(telemetry.End{At: time.Now()})
	cancels := ev.Occurrences["git.cancels"]
	if len(cancels) != 1 {
		t.Fatalf("git.cancels = %v, want one occurrence", cancels)
	}
	// Read it as the events file does: through JSON.
	line, err := json.Marshal(cancels[0])
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Cause     *string `json:"git.cancel.cause"`
		Processes int     `json:"git.cancel.processes"`
		Survivors *int    `json:"git.cancel.survivors"`
		KillError *string `json:"git.cancel.kill_error"`
		OutputCut *bool   `json:"git.cancel.output_cut"`
	}
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatal(err)
	}
	if got.Cause == nil || *got.Cause != context.Canceled.Error() {
		t.Fatalf("cause = %v, want %q (%s)", got.Cause, context.Canceled, line)
	}
	if got.Processes < 1 || got.Survivors == nil || *got.Survivors != 0 || got.KillError != nil || got.OutputCut == nil || *got.OutputCut {
		t.Fatalf("occurrence = %s, want at least git ended, 0 survivors, no kill error, output not cut", line)
	}
}

// An invocation that ended no git says so: git.cancels is [], not absent.
func TestInvocationWithNoCancelledGitRecordsNone(t *testing.T) {
	recorder := telemetry.Begin(telemetry.Event{})
	invocationContext(context.Background(), recorder)
	line, err := telemetry.Line(recorder.Finish(telemetry.End{At: time.Now()}))
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Occurrences map[string]json.RawMessage `json:"occurrences"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatal(err)
	}
	if got := string(ev.Occurrences["git.cancels"]); got != "[]" {
		t.Fatalf("git.cancels = %q, want []", got)
	}
}
