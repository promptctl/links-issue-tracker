// Package storeopen opens the store directly.
package storeopen

import (
	"testing"

	"example.com/fixtures/internal/store"
)

// store.Open is a marker: the store, and nothing past it.
func TestOpenTouchesTheStore(t *testing.T) {
	_, _ = store.Open("unused")
}
