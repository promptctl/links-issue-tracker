// Package httpclient sends requests through a port an outside client
// implements.
package httpclient

import "net/http"

type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

func Fetch(d Doer, req *http.Request) (*http.Response, error) { return d.Do(req) }
