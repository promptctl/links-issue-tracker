package climain

import (
	"os"
	"testing"
)

type executor interface{ Execute(args []string) int }

var runner executor

func TestMain(m *testing.M) {
	runner = Command{}
	if os.Getenv("CLIMAIN_RUN") == "1" {
		os.Exit(Run(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func TestThroughThePort(t *testing.T) { _ = runner.Execute(nil) }
