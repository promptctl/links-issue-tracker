// Package varblank asserts at compile time that a touching type implements
// its port, and never makes one.
package varblank

import "os"

type Opener interface{ Open(dir string) error }

type disk struct{}

func (*disk) Open(dir string) error { return os.MkdirAll(dir, 0o755) }

var _ Opener = (*disk)(nil)

// OpenAny opens o, when there is one.
func OpenAny(o Opener) error {
	if o == nil {
		return nil
	}
	return o.Open("")
}
