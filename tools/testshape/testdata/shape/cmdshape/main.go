// Command cmdshape is shaped like a CLI's main package: its test binary runs
// main when asked, and its tests run the binary and git through one helper.
package main

import "os"

func main() {
	if _, err := os.ReadFile("config"); err != nil {
		os.Exit(1)
	}
}
