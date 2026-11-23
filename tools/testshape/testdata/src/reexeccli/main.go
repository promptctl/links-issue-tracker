// Command reexeccli is a CLI whose tests re-run their binary as it.
package main

import (
	"os"

	"example.com/fixtures/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		os.Exit(1)
	}
}
