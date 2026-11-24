package main

import (
	"os"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/cli"
	"github.com/promptctl/links-issue-tracker/internal/deadline"
)

// The run this process is starts its detached workers as this very program,
// under the deadlines lit ships with: a worker is the binary that spawned it.
func TestTheProcessRunStartsItsWorkersAsItself(t *testing.T) {
	t.Parallel()
	inv, err := processInvocation()
	if err != nil {
		t.Fatalf("processInvocation() error = %v", err)
	}
	program, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	if want := cli.LaunchWorkersAs(program); inv.Workers != want {
		t.Errorf("processInvocation().Workers = %#v; want %#v, this program", inv.Workers, want)
	}
	if got, want := inv.Env.Deadlines(), deadline.Shipped(); got != want {
		t.Errorf("processInvocation()'s deadlines = %+v; want the shipped %+v", got.Figures(), want.Figures())
	}
}
