// Package store is the fixture module's store on disk: its Open is a store
// marker, so nothing past it is walked.
package store

import "os"

// Store is a store on disk.
type Store struct{ dir string }

// Open opens the store in dir.
func Open(dir string) (*Store, error) {
	return &Store{dir}, os.MkdirAll(dir, 0o755)
}

// Get reads nothing past what Open opened.
func (s *Store) Get() string { return s.dir }
