// Package app assembles the fixture module's store behind an interface, as a
// composition root does.
package app

import "example.com/fixtures/internal/store"

// Store is the contract a test reaches the store through.
type Store interface{ Get() string }

// App holds the store as its contract only.
type App struct{ Store Store }

// New builds the store on disk and erases its type. It is no marker, so the
// walk enters it.
func New(dir string) (*App, error) {
	st, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	return &App{Store: st}, nil
}

// Open and OpenLocationForRead are the openers markers.go names.
func Open(dir string) (*App, error)                { return New(dir) }
func OpenLocationForRead(dir string) (*App, error) { return New(dir) }
