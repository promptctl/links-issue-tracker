// schema-release-check refuses a source install of lit whose schema no
// published release supports. scripts/install.sh's source mode builds and runs
// it before building lit, passing the newest release tag in this checkout as
// its one argument: the empty string when the checkout has none.
//
// The tree's schema is migrations.MaxVersion(): this binary is built from the
// same tree as the lit about to be installed, so it embeds the same registry.
// The release's schema is the highest migration in the tag's
// internal/store/migrations, the files that release's binary embedded.
//
// Every run prints one comparison line on stdout, whatever the verdict:
//
//	schema-release-check: tree=10 release_tag=v0.18.0 release_schema=9 verdict=refuse
//
// Exit codes: 0, install; 3, the tree's schema is ahead of the newest release;
// 1, the tree cannot be shown released (no release tag, or the comparison could
// not be made). CI's install step accepts 3 and nothing else, because a merged
// migration leaves master ahead until its release is cut.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

const (
	exitInstall    = 0
	exitUnknown    = 1
	exitTreeAhead  = 3
	registryInTree = "internal/store/migrations/"
	latestRelease  = "scripts/install.sh --latest-release"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: schema-release-check <newest release tag, or \"\" when there is none>")
		os.Exit(exitUnknown)
	}
	os.Exit(run(os.Args[1], os.Stdout, os.Stderr))
}

// outcome is one run's comparison and verdict. A value the run could not
// establish reads "none" (there is no release tag) or "unknown" (reading it
// failed), so the comparison line always carries all three.
type outcome struct {
	tree, tag, schema string
	exit              int
	refusal           string // stderr text; empty exactly when exit is exitInstall
}

// run prints the comparison on stdout every time, then any refusal on stderr.
// [LAW:nothing-unseen] a passing install shows the comparison it passed.
func run(tag string, stdout, stderr io.Writer) int {
	o := compare(tag, stderr)
	verdict := "install"
	if o.exit != exitInstall {
		verdict = "refuse"
	}
	fmt.Fprintf(stdout, "schema-release-check: tree=%s release_tag=%s release_schema=%s verdict=%s\n", o.tree, o.tag, o.schema, verdict)
	fmt.Fprint(stderr, o.refusal)
	return o.exit
}

func compare(tag string, stderr io.Writer) outcome {
	tree, err := migrations.MaxVersion()
	if err != nil {
		return outcome{tree: "unknown", tag: or(tag, "none"), schema: "unknown", exit: exitUnknown,
			refusal: fmt.Sprintf("error: schema-release-check: %v\n", err)}
	}
	if tag == "" {
		return outcome{tree: fmt.Sprint(tree), tag: "none", schema: "none", exit: exitUnknown,
			refusal: fmt.Sprintf("error: this checkout has no release tag (vX.Y.Z), so nothing shows its schema (%d) is released; lit was not installed.\n"+
				"       Fetch the release tags (git fetch origin --tags), or install a release: %s\n", tree, latestRelease)}
	}
	released, err := releaseSchema(tag, stderr)
	if err != nil {
		return outcome{tree: fmt.Sprint(tree), tag: tag, schema: "unknown", exit: exitUnknown,
			refusal: fmt.Sprintf("error: schema-release-check: %v\n", err)}
	}
	o := outcome{tree: fmt.Sprint(tree), tag: tag, schema: fmt.Sprint(released), exit: exitInstall}
	if tree > released {
		o.exit = exitTreeAhead
		o.refusal = fmt.Sprintf("error: this tree's migrations reach schema %d, and the newest release, %s, supports schema %d at most.\n"+
			"       A lit built from it would migrate a workspace to a schema no release can open, so it was not installed.\n"+
			"       Install a release: %s\n", tree, tag, released, latestRelease)
	}
	return o
}

func or(s, empty string) string {
	if s == "" {
		return empty
	}
	return s
}

// releaseSchema is the highest migration version in tag's registry.
func releaseSchema(tag string, stderr io.Writer) (int64, error) {
	var out bytes.Buffer
	cmd := exec.Command("git", "ls-tree", "--name-only", "refs/tags/"+tag, "--", registryInTree)
	cmd.Stdout, cmd.Stderr = &out, stderr
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("list %s at %s: %w", registryInTree, tag, err)
	}
	v, err := maxSchema(strings.Fields(out.String()))
	if err != nil {
		return 0, fmt.Errorf("%s at %s: %w", registryInTree, tag, err)
	}
	return v, nil
}

// maxSchema applies migrations.MaxVersion's registry rule to file names: the
// registry is the *.sql files, every one carries a version, and it is never
// empty. [LAW:one-source-of-truth] migrations.ParseVersion reads each version;
// TestMaxSchemaAgreesWithTheEmbeddedRegistry holds this rule to MaxVersion's.
func maxSchema(names []string) (int64, error) {
	var highest int64
	for _, name := range names {
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		v, ok := migrations.ParseVersion(name)
		if !ok {
			return 0, fmt.Errorf("migration file %q does not begin with a numeric version", name)
		}
		highest = max(highest, v)
	}
	if highest == 0 {
		return 0, errors.New("migration registry is empty")
	}
	return highest, nil
}
