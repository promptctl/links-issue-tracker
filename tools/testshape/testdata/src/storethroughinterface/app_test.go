// Package storethroughinterface reaches a store only through app.Store.
package storethroughinterface

import (
	"testing"

	"example.com/fixtures/internal/app"
)

type memStore struct{}

func (memStore) Get() string { return "" }

// The store is reached through the interface; app.New, which built it,
// carries the touch.
func TestTheConstructorCarriesTheStore(t *testing.T) {
	ap, err := app.New("unused")
	if err != nil {
		t.Fatal(err)
	}
	_ = ap.Store.Get()
}

// The same call through the interface, on a store in memory.
func TestAnAppOnAMemoryStoreTouchesNothing(t *testing.T) {
	ap := &app.App{Store: memStore{}}
	_ = ap.Store.Get()
}
