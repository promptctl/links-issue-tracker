// lawtokens-check refuses a commit that cites a law token outside the
// canonical index. The pre-commit framework runs it (.pre-commit-config.yaml)
// with the staged files as arguments, paths relative to the repository root.
//
// It runs lawtokens.CheckFiles, the same check TestRepoMarkersAreCanonical
// runs over the whole tree in CI; this only moves the refusal to the moment
// of commit, before an invented token can be pushed or copied.
// [LAW:single-enforcer]
//
// Exit codes: 0 when every marker is canonical, 1 when any is not, 2 when a
// file could not be read.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/promptctl/links-issue-tracker/internal/lawtokens"
)

func main() {
	os.Exit(run(os.DirFS("."), os.Args[1:], os.Stderr))
}

func run(fsys fs.FS, paths []string, stderr io.Writer) int {
	violations, err := lawtokens.CheckFiles(fsys, paths)
	if err != nil {
		fmt.Fprintln(stderr, "lawtokens-check:", err)
		return 2
	}
	if len(violations) > 0 {
		fmt.Fprint(stderr, lawtokens.Report(violations))
		return 1
	}
	return 0
}
