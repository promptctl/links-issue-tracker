// Package environ is the fixture module's environment: its Env.Git is a git
// marker, so nothing past it is walked.
package environ

import "example.com/fixtures/internal/store"

// Env is the process environment.
type Env struct{}

// Git runs git with args; its body reaches the store, which the marker
// stops the walk short of.
func (Env) Git(args ...string) error {
	_, err := store.Open("unused")
	return err
}
