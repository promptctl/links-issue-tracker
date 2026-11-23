// Package climain is a CLI that also implements a port, and whose test
// binary's TestMain runs it in one branch.
package climain

import "os"

// Run is the CLI.
func Run(args []string) int {
	if _, err := os.ReadFile("config"); err != nil {
		return 1
	}
	return 0
}

// Command is the CLI behind a port.
type Command struct{}

func (Command) Execute(args []string) int { return Run(args) }
