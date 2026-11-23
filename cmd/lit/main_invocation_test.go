package main

import (
	"os"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/cli"
	"github.com/promptctl/links-issue-tracker/internal/store"
)

// The run this process is starts its detached workers as this very program,
// under the deadlines lit ships with: a worker is the binary that spawned it.
func TestTheProcessRunStartsItsWorkersAsItself(t *testing.T) {
	t.Parallel()
	inv, err := processInvocation()
	if err != nil {
		t.Fatalf("processInvocation() error = %v", err)
	}
	if want := cli.LaunchWorkersAs(os.Executable()); inv.Workers != want {
		t.Errorf("processInvocation().Workers = %#v; want %#v, this program", inv.Workers, want)
	}
	if want := store.ShippedDeadlines(); inv.Deadlines != want {
		t.Errorf("processInvocation().Deadlines = %+v; want the shipped %+v", inv.Deadlines, want)
	}
}
