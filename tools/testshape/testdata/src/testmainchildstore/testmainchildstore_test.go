package testmainchildstore

import (
	"os"
	"testing"
)

// TestMain's child branch stores a no-op and runs it; the parent runs the
// tests. TestMain itself reaches hook, so its row carries the filesystem.
func TestMain(m *testing.M) {
	if os.Getenv("FIXTURE_CHILD") == "1" {
		hook = func() {}
		Run()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestMain's store is no initializer's: the test still reaches the file.
func TestAHookTestMainReplacesStillTouchesTheFilesystem(t *testing.T) { Run() }
