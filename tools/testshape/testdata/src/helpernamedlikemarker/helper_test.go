// Package helpernamedlikemarker has test helpers named like the module's
// markers, store.Open and cli.Run.
package helpernamedlikemarker

import "testing"

func Open(dir string) (string, error) { return dir, nil }

func Run(args []string) error { return nil }

func TestHelpersNamedLikeMarkersTouchNothing(t *testing.T) {
	_, _ = Open("")
	_ = Run(nil)
}
