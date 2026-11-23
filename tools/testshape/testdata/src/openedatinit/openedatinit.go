// Package openedatinit opens a file in a package variable's initializer and
// reads it through a method of the value.
package openedatinit

import "os"

// Store counts what its file holds.
type Store struct{ f *os.File }

func openStore() *Store {
	f, _ := os.Open("x")
	return &Store{f}
}

var db = openStore()

// Count reads nothing itself: the file is the one openStore opened.
func (s *Store) Count() int { return 0 }
