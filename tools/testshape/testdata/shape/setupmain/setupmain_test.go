package setupmain

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if err := os.MkdirAll("scratch", 0o755); err != nil {
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Fail()
	}
}
