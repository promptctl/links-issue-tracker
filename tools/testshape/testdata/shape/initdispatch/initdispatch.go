// Package initdispatch registers a function with an outside dispatcher during
// initialization, through a parameter whose type cannot carry it.
package initdispatch

import (
	"expvar"
	"os"
)

func init() { expvar.Publish("config", expvar.Func(config)) }

func config() any {
	b, _ := os.ReadFile("config")
	return string(b)
}

// Serve renders whatever was registered.
func Serve() string { return expvar.Get("config").String() }
