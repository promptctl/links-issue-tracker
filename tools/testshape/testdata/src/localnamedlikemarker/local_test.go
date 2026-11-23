// Package localnamedlikemarker has block-scoped locals spelled like markers.
package localnamedlikemarker

import "testing"

type clock struct{}

func (clock) Now() int { return 0 }

// exec.Command("git") and time.Now() here are a local's field and method.
func TestLocalsSpelledLikeMarkersTouchNothing(t *testing.T) {
	exec := struct{ Command func(string) string }{Command: func(s string) string { return s }}
	_ = exec.Command("git")
	time := clock{}
	_ = time.Now()
}
