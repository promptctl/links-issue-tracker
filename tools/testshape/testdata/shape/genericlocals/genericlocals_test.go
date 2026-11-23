package genericlocals

import "testing"

func TestBoth(t *testing.T) {
	_ = Read()
	_ = Write()
}
