// Package initdispatch registers a handler with an outside dispatcher during
// initialization.
package initdispatch

import (
	"net/http"
	"os"
)

func init() { http.HandleFunc("/config", serveConfig) }

func serveConfig(w http.ResponseWriter, r *http.Request) {
	b, _ := os.ReadFile("config")
	w.Write(b)
}

// Serve hands a request to whatever was registered.
func Serve(w http.ResponseWriter, r *http.Request) { http.DefaultServeMux.ServeHTTP(w, r) }
