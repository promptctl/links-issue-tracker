// Package skipped has a touching test the run skips beside a unit test that
// runs.
package skipped

import "os"

func Read() ([]byte, error) { return os.ReadFile("config") }
