package buildsgo

import "testing"

func TestBuildsThenRuns(t *testing.T) {
	_ = Build("cli")
	_ = RunTool()
}

func TestRunsOnly(t *testing.T) { _ = RunTool() }
