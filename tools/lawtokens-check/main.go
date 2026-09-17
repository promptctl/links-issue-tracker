// lawtokens-check refuses a commit that cites a law token outside the
// canonical index. The pre-commit framework runs it (.pre-commit-config.yaml)
// with the staged files as arguments.
//
// It runs lawtokens.CheckFiles, the same check TestRepoMarkersAreCanonical
// runs over the whole tree in CI; this only moves the refusal to the moment
// of commit, before an invented token can be pushed or copied.
// [LAW:single-enforcer]
//
// It exits 0 when every marker is canonical and 1 otherwise. There is one
// failure code because the hook runs it through `go run`, which reports every
// non-zero exit as 1; the message on stderr says what failed.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/lawtokens"
)

func main() {
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "lawtokens-check:", err)
		os.Exit(1)
	}
	os.Exit(run(root, os.Args[1:], os.Stderr))
}

func run(root string, args []string, stderr io.Writer) int {
	names, err := fsNames(root, args)
	if err != nil {
		fmt.Fprintln(stderr, "lawtokens-check:", err)
		return 1
	}
	violations, err := lawtokens.CheckFiles(os.DirFS(root), names)
	if err != nil {
		fmt.Fprintln(stderr, "lawtokens-check:", err)
		return 1
	}
	if len(violations) > 0 {
		fmt.Fprint(stderr, lawtokens.Report(violations))
		return 1
	}
	return 0
}

// fsNames turns command-line paths into names inside root that io/fs accepts.
// An fs.FS takes only clean, slash-separated, relative names, so "./a.go",
// "sub/../a.go" and an absolute path under root would otherwise all fail to
// read. [LAW:parse-dont-validate] A path outside root is refused, not skipped.
func fsNames(root string, args []string) ([]string, error) {
	names := make([]string, len(args))
	for i, arg := range args {
		abs := arg
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, abs)
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%s is outside the working directory %s", arg, root)
		}
		names[i] = filepath.ToSlash(rel)
	}
	return names, nil
}
