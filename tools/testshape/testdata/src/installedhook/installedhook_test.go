package installedhook

import "testing"

// install's parameter is resolved where init calls it: git.
func TestAHookAHelperInstalledRunsGit(t *testing.T) { _ = Run() }
