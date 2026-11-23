package skipped

import "testing"

func TestRead(t *testing.T) {
	t.Skip("needs a config file")
	_, _ = Read()
}

func TestPure(t *testing.T) {}
