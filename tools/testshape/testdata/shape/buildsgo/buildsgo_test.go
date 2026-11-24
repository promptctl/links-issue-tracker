package buildsgo

import "testing"

func TestBuildsThenRuns(t *testing.T) {
	_ = Build("cli")
	_ = RunTool()
}

func TestCompilesATestThenRuns(t *testing.T) {
	_ = CompileTest("cli.test")
	_ = RunTool()
}

func TestListsThenRuns(t *testing.T) {
	_ = List()
	_ = RunTool()
}

func TestTestsThenRuns(t *testing.T) {
	_ = Test()
	_ = RunTool()
}

func TestRunsAnUnresolvedSubcommandThenRuns(t *testing.T) {
	_ = Go()
	_ = RunTool()
}

func TestRunsOnly(t *testing.T) { _ = RunTool() }
