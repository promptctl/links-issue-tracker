// schema-release-check refuses a source install of lit whose schema no
// published release supports. scripts/install.sh's source mode builds and runs
// it before building lit, passing the newest release tag in this checkout as
// its one argument: the empty string when the checkout has none.
//
// A tree is released when every migration it embeds is in the tag's
// internal/store/migrations byte for byte: the files that release's binary
// embedded. This binary is built from the same tree as the lit about to be
// installed, so it embeds the same registry. Comparing bytes, not only the
// highest version, refuses a tree whose migration shares a version with the
// release's but not its SQL: a lit built from it would record that version for
// SQL no release ran.
//
// Every run prints one comparison line on stdout, whatever the verdict:
//
//	schema-release-check: tree=10 release_tag=v0.18.0 release_schema=9 verdict=refuse
//
// Exit codes: 0, install; 3, the tree carries a migration the newest release
// does not; 1, the tree cannot be shown released (no release tag, or the
// comparison could not be made). CI's install step accepts 3 and nothing else,
// because a merged migration leaves master ahead until its release is cut.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
	"github.com/promptctl/links-issue-tracker/tools/internal/releasedschema"
)

const (
	exitInstall   = 0
	exitUnknown   = 1
	exitTreeAhead = 3
	latestRelease = "scripts/install.sh --latest-release"
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
	embedded, err := migrations.Embedded()
	if err != nil {
		return outcome{tree: "unknown", tag: or(tag, "none"), schema: "unknown", exit: exitUnknown,
			refusal: fmt.Sprintf("error: schema-release-check: %v\n", err)}
	}
	tree := embedded.MaxVersion()
	if tag == "" {
		return outcome{tree: fmt.Sprint(tree), tag: "none", schema: "none", exit: exitUnknown,
			refusal: fmt.Sprintf("error: this checkout has no release tag (vX.Y.Z), so nothing shows its schema (%d) is released; lit was not installed.\n"+
				"       In a git clone, fetch the release tags (git fetch origin --tags); or install a release: %s\n", tree, latestRelease)}
	}
	released, unreleased, err := releasedschema.Released(tag, embedded, stderr)
	if err != nil {
		return outcome{tree: fmt.Sprint(tree), tag: tag, schema: "unknown", exit: exitUnknown,
			refusal: fmt.Sprintf("error: schema-release-check: %v\n", err)}
	}
	o := outcome{tree: fmt.Sprint(tree), tag: tag, schema: fmt.Sprint(released), exit: exitInstall}
	if len(unreleased) > 0 {
		o.exit = exitTreeAhead
		o.refusal = fmt.Sprintf("error: this tree's migrations reach schema %d, and the newest release, %s, supports schema %d at most.\n"+
			"       %s does not carry these migrations as this tree has them: %s.\n"+
			"       A lit built from this tree would migrate a workspace to a schema no release supports, so it was not installed.\n"+
			"       Install a release: %s\n", tree, tag, released, tag, strings.Join(unreleased, ", "), latestRelease)
	}
	return o
}

func or(s, empty string) string {
	if s == "" {
		return empty
	}
	return s
}
