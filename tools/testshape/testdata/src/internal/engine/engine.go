// Package engine is the fixture module's engine: its Open is a store marker.
package engine

import "example.com/fixtures/internal/store"

// Open opens the store in dir.
func Open(dir string) (*store.Store, error) { return store.Open(dir) }
