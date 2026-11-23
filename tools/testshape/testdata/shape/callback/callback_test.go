package callback

import (
	"net/url"
	"testing"
)

func TestPrint(t *testing.T) {
	_ = Print(&url.URL{Scheme: "https", Host: "example.com"})
}
