// Package releasedschema answers which of this tree's migrations a release has
// shipped: the release's registry is the internal/store/migrations directory at
// its tag, and a migration is shipped when the tag holds its bytes exactly.
//
// [LAW:one-source-of-truth] the one definition of "unreleased migration", read
// by schema-release-check (may this tree be installed?) and schema-compat-check
// (may this tree's schema be published?).
package releasedschema

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"path"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/store/migrations"
)

const registryInTree = "internal/store/migrations/"

// Released is the highest migration version in tag's registry, and the
// migrations this tree embeds that tag's registry does not hold byte for byte.
// git runs in the working directory, which must be a checkout holding tag.
func Released(tag string, embedded migrations.Registry, stderr io.Writer) (int64, []string, error) {
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
