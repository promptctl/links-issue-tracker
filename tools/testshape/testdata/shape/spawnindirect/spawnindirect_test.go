package spawnindirect

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("SPAWNINDIRECT_RUN") == "1" {
		Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestStatusAndRerun(t *testing.T) {
	_ = Status()
	_ = Rerun()
}
