// Package initdispatch takes a function as a value during initialization,
// handing it to no outside code, and renders through an outside dispatcher,
// which can call any function taken as a value.
package initdispatch

import (
	"expvar"
	"os"
)

var handlers = map[string]func() any{"config": config}

func init() { expvar.Publish("hits", new(expvar.Int)) }

func config() any {
	b, _ := os.ReadFile("config")
	return string(b)
}

// Serve renders what was published.
func Serve() string { return expvar.Get("hits").String() }
