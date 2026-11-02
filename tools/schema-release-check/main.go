// schema-release-check refuses a source install of lit whose schema no
// published release supports. scripts/install.sh's source mode builds and runs
// it before building lit, passing the newest release tag in this checkout as
// its one argument: the empty string when the checkout has none.
//
// A tree is released when every migration it embeds is in the tag's
// internal/store/migrations under the same migrations.UpIdentity: the Ups that
// release's binary ran. This binary is built from the same tree as the lit
// about to be installed, so it embeds the same registry. Comparing Ups, not
// only the highest version, refuses a tree whose migration shares a version
// with the release's but not its Up SQL: a lit built from it would record that
// version for SQL no release ran. A Down fixed in place changes no Up, so it
// leaves its migration released.
//
// Every run prints one comparison line on stdout, whatever the verdict:
//
//	schema-release-check: tree=10 release_tag=v0.18.0 release_schema=9 verdict=refuse
//
// Exit codes: 0, install; 3, the tree carries a migration the newest release
// does not; 1, the tree cannot be shown released (no release tag, or the
// comparison could not be made). CI's install step accepts 3 and nothing else,
// because a merged migration leaves master ahead until its release is cut.
//
// With -released-schema, it instead prints the released-schema ceiling of this
// tree against the tag and nothing else (printReleasedSchema): the number every
// from-source build stamps into internal/version.ReleasedSchema
// (scripts/version-ldflags.sh). An empty tag prints 0: with no release known,
// no schema is released. It exits 1 when the comparison cannot be made.
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
	if len(os.Args) == 3 && os.Args[1] == "-released-schema" {
		os.Exit(printReleasedSchema(os.Args[2], os.Stdout, os.Stderr))
	}
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

// printReleasedSchema prints the released-schema ceiling of this tree against
// tag: the highest version N such that every migration this tree carries at or
// below N is in tag's registry with the same Up, and N is no higher than tag's own
// highest. A migration edited, or re-added, under a released number lowers the
// ceiling below that number. No tag is ceiling 0.
// [LAW:one-source-of-truth] it reads the install guard's own comparison
// (releaseSchema), so the guard and the ceiling cannot disagree about what is
// released.
func printReleasedSchema(tag string, stdout, stderr io.Writer) int {
	if tag == "" {
		fmt.Fprintln(stdout, 0)
		return exitInstall
	}
	ceiling, err := releasedCeiling(tag, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "error: schema-release-check: %v\n", err)
		return exitUnknown
	}
	fmt.Fprintln(stdout, ceiling)
	return exitInstall
}

// releasedCeiling is printReleasedSchema's number.
func releasedCeiling(tag string, stderr io.Writer) (int64, error) {
	embedded, err := migrations.Embedded()
	if err != nil {
		return 0, err
	}
	ceiling, unreleased, err := releaseSchema(tag, embedded, stderr)
	if err != nil {
		return 0, err
	}
	for _, name := range unreleased {
		version, ok := migrations.ParseVersion(name)
		if !ok {
			return 0, fmt.Errorf("embedded migration %s has no version", name)
		}
		ceiling = min(ceiling, version-1)
	}
	return ceiling, nil
}

func or(s, empty string) string {
	if s == "" {
		return empty
	}
	return s
}

// releaseSchema is the highest migration version in tag's registry, and the
// migrations this tree embeds that tag's registry does not hold with the same Up.
func releaseSchema(tag string, embedded migrations.Registry, stderr io.Writer) (int64, []string, error) {
	release, err := releaseUps(tag, stderr)
	if err != nil {
		return 0, nil, err
	}
	registry, err := parseReleaseRegistry(tag, release)
	if err != nil {
		return 0, nil, err
	}
	tree, err := treeUps(embedded)
	if err != nil {
		return 0, nil, err
	}
	return registry.MaxVersion(), unreleasedMigrations(tree, release), nil
}

// releaseUps is the UpIdentity of every migration file in tag's registry.
func releaseUps(tag string, stderr io.Writer) (upIDs, error) {
	var out bytes.Buffer
	cmd := exec.Command("git", "ls-tree", "-z", "refs/tags/"+tag, "--", registryInTree)
	cmd.Stdout, cmd.Stderr = &out, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("list %s at %s: %w", registryInTree, tag, err)
	}
	blobs, err := parseLsTree(out.String())
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", registryInTree, tag, err)
	}
	ups := upIDs{}
	for name, blob := range blobs {
		var content bytes.Buffer
		cmd := exec.Command("git", "cat-file", "blob", blob)
		cmd.Stdout, cmd.Stderr = &content, stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", name, tag, err)
		}
		ups[name] = migrations.UpIdentity(content.Bytes())
	}
	return ups, nil
}

// parseReleaseRegistry reads release's file names by the rule the runner reads
// its own: migrations.ParseRegistry. [LAW:one-source-of-truth]
func parseReleaseRegistry(tag string, release upIDs) (migrations.Registry, error) {
	registry, err := migrations.ParseRegistry(slices.Collect(maps.Keys(release)))
	if err != nil {
		return migrations.Registry{}, fmt.Errorf("%s at %s: %w", registryInTree, tag, err)
	}
	return registry, nil
}

// upIDs maps a migration file's name to its migrations.UpIdentity.
type upIDs map[string]string

// parseLsTree reads `git ls-tree -z` output, NUL-terminated
// "<mode> <type> <id>\t<path>" entries, into each file's blob id.
func parseLsTree(out string) (map[string]string, error) {
	ids := map[string]string{}
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

// treeUps is the UpIdentity of every migration this binary embeds.
func treeUps(embedded migrations.Registry) (upIDs, error) {
	ups := upIDs{}
	for _, m := range embedded.Migrations() {
		content, err := migrations.FS.ReadFile(m.Name)
		if err != nil {
			return nil, fmt.Errorf("read embedded migration %s: %w", m.Name, err)
		}
		ups[m.Name] = migrations.UpIdentity(content)
	}
	return ups, nil
}

// unreleasedMigrations is every tree migration the release lacks or holds a
// different Up for, in name order. Empty exactly when the release carries
// the whole tree's registry, which bounds the tree's schema by the release's.
func unreleasedMigrations(tree, release upIDs) []string {
	var unreleased []string
	for name, id := range tree {
		if release[name] != id {
			unreleased = append(unreleased, name)
		}
	}
	slices.Sort(unreleased)
	return unreleased
}
