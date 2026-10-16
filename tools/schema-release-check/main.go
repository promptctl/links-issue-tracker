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
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"slices"
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
	released, unreleased, err := releaseSchema(tag, embedded, stderr)
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

// releaseSchema is the highest migration version in tag's registry, and the
// migrations this tree embeds that tag's registry does not hold byte for byte.
func releaseSchema(tag string, embedded migrations.Registry, stderr io.Writer) (int64, []string, error) {
	var out bytes.Buffer
	cmd := exec.Command("git", "ls-tree", "-z", "refs/tags/"+tag, "--", registryInTree)
	cmd.Stdout, cmd.Stderr = &out, stderr
	if err := cmd.Run(); err != nil {
		return 0, nil, fmt.Errorf("list %s at %s: %w", registryInTree, tag, err)
	}
	release, err := parseLsTree(out.String())
	if err != nil {
		return 0, nil, fmt.Errorf("%s at %s: %w", registryInTree, tag, err)
	}
	// [LAW:one-source-of-truth] the release's registry is read by the rule the
	// runner's is: migrations.ParseRegistry.
	releaseRegistry, err := migrations.ParseRegistry(slices.Collect(maps.Keys(release)))
	if err != nil {
		return 0, nil, fmt.Errorf("%s at %s: %w", registryInTree, tag, err)
	}
	tree, err := treeRegistry(embedded, stderr)
	if err != nil {
		return 0, nil, err
	}
	return releaseRegistry.MaxVersion(), unreleasedMigrations(tree, release), nil
}

// blobIDs maps a migration file's name to its git blob id.
type blobIDs map[string]string

// parseLsTree reads `git ls-tree -z` output: NUL-terminated
// "<mode> <type> <id>\t<path>" entries.
func parseLsTree(out string) (blobIDs, error) {
	ids := blobIDs{}
	for _, entry := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		meta, file, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("unreadable git ls-tree entry %q", entry)
		}
		ids[path.Base(file)] = fields[2]
	}
	return ids, nil
}

// treeRegistry is the blob id of every migration this binary embeds, as git
// hashes them. --no-filters hashes the embedded bytes as they are.
func treeRegistry(embedded migrations.Registry, stderr io.Writer) (blobIDs, error) {
	ids := blobIDs{}
	for _, m := range embedded.Migrations() {
		content, err := migrations.FS.ReadFile(m.Name)
		if err != nil {
			return nil, fmt.Errorf("read embedded migration %s: %w", m.Name, err)
		}
		var out bytes.Buffer
		cmd := exec.Command("git", "hash-object", "--no-filters", "--stdin")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(content), &out, stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("hash embedded migration %s: %w", m.Name, err)
		}
		ids[m.Name] = strings.TrimSpace(out.String())
	}
	return ids, nil
}

// unreleasedMigrations is every tree migration the release lacks or holds
// different bytes for, in name order. Empty exactly when the release carries
// the whole tree's registry, which bounds the tree's schema by the release's.
func unreleasedMigrations(tree, release blobIDs) []string {
	var unreleased []string
	for name, id := range tree {
		if release[name] != id {
			unreleased = append(unreleased, name)
		}
	}
	slices.Sort(unreleased)
	return unreleased
}
