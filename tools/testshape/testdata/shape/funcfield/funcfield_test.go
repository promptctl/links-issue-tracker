package funcfield

import (
	"io"
	"testing"
)

func TestNew(t *testing.T) {
	_ = New(io.Discard)
}
