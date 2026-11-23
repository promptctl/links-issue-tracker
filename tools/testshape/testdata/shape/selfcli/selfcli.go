// Package selfcli is a CLI whose tests run their own test binary as it.
package selfcli

import "os"

// Run is the CLI.
func Run(args []string) int {
	if _, err := os.ReadFile("config"); err != nil {
		return 1
	}
	return 0
}
