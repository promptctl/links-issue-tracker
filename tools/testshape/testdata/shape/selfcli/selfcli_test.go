package selfcli

import (
	"os"
	"os/exec"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("SELFCLI_RUN") == "1" {
		os.Exit(Run(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func TestRerun(t *testing.T) {
	cmd := exec.Command(os.Args[0], "status")
	cmd.Env = append(os.Environ(), "SELFCLI_RUN=1")
	_ = cmd.Run()
}
