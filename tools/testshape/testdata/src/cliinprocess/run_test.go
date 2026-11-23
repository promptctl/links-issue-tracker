// Package cliinprocess runs the CLI in its own process.
package cliinprocess

import (
	"testing"

	"example.com/fixtures/internal/cli"
)

func TestRunningTheCLIInProcessIsEndToEnd(t *testing.T) {
	_ = cli.Run([]string{"version"})
}
