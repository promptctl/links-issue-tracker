package httpclient

import (
	"net/http"
	"testing"
)

func TestFetch(t *testing.T) { _, _ = Fetch(&http.Client{}, nil) }
