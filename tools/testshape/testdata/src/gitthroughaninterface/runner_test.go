package gitthroughaninterface

import "testing"

type fakeRunner struct{}

func (fakeRunner) Run() error { return nil }

// Drive calls Run through the interface; the fake the test builds touches
// nothing, and the git runner is built by no one here.
func TestDriveHandedAFakeTouchesNothing(t *testing.T) {
	_ = Drive(fakeRunner{})
}

// NewGitRunner converts the git runner to the interface, so it carries git.
func TestTheConstructorCarriesGit(t *testing.T) {
	_ = Drive(NewGitRunner(""))
}

// The test converts a composite literal to the interface itself.
func TestACompositeLiteralThroughTheInterfaceTouchesGit(t *testing.T) {
	var r Runner = gitRunner{dir: ""}
	_ = Drive(r)
}
