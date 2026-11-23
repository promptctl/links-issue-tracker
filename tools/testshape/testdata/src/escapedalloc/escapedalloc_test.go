// Package escapedalloc fills its spawned program through a pointer, so the
// local that names it escapes.
package escapedalloc

import (
	"os/exec"
	"testing"
)

func fill(p *string) { *p = "git" }

func TestAProgramFilledThroughAPointerIsUnresolved(t *testing.T) {
	var prog string
	fill(&prog)
	_ = exec.Command(prog).Run()
}
