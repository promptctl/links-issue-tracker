// Package externalinterface calls an *http.Client through an interface
// variable of its own.
package externalinterface

import "net/http"

type doer interface {
	Do(*http.Request) (*http.Response, error)
}

var d doer = http.DefaultClient

// Fetch sends req through d.
func Fetch(req *http.Request) { _, _ = d.Do(req) }
