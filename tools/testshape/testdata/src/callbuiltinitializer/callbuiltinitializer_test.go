package callbuiltinitializer

import "testing"

func TestAClockBuiltByACallReadsTheClock(t *testing.T) { _ = Stamp() }

func TestAHookBuiltByACallRunsGit(t *testing.T) { _ = Run() }
