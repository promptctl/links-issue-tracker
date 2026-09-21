package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// generatedStore is a lit workspace that exists at a known row count, with the
// path of the store on disk resolved rather than assumed.
type generatedStore struct {
	size size
	// root is the workspace directory — the cwd every probe is invoked from.
	root string
	// databasePath is the store directory whose bytes get reported. It comes
	// from workspace.Resolve, the package that owns lit's path geometry, so
	// this tool cannot drift from lit's own idea of where the store lives by
	// hardcoding ".git/links/dolt". [LAW:one-source-of-truth]
	databasePath string
}

// importRecord is one issue in the JSON tree spec `lit import` consumes. The
// field names and the local_id/depends_on referencing rules are that command's
// contract, not this tool's invention.
type importRecord struct {
	LocalID     string   `json:"local_id"`
	Title       string   `json:"title"`
	Type        string   `json:"type"`
	Topic       string   `json:"topic"`
	Description string   `json:"description"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

// generate builds a workspace at the requested size under parent.
//
// WHY IT SHELLS OUT INSTEAD OF CALLING internal/store. Seeding through the
// store API would be faster, and it would also be a second definition of what a
// populated lit store looks like — one that drifts from the real write path the
// moment a migration, a default, or a rank rule changes, and drifts silently,
// because nothing would compare the two. `lit init` plus `lit import` is the
// bulk-ingest path lit already offers its users, so the store measured here is
// a store lit itself produced. [LAW:one-source-of-truth]
func generate(bin litBinary, parent string, sz size) (generatedStore, error) {
	root := filepath.Join(parent, sz.name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return generatedStore{}, fmt.Errorf("creating workspace dir: %w", err)
	}
	// lit resolves its store relative to a git checkout, so the workspace has
	// to be one. The identity is passed per-command rather than written into a
	// config file so generation cannot depend on — or disturb — whatever git
	// identity the machine running this has.
	steps := [][]string{
		{"git", "init", "-q", "."},
		{"git", "-c", "user.email=perfbench@invalid", "-c", "user.name=perfbench",
			"commit", "-q", "--allow-empty", "-m", "perfbench workspace"},
	}
	for _, step := range steps {
		if err := runQuiet(root, step[0], step[1:]...); err != nil {
			return generatedStore{}, err
		}
	}
	// --skip-hooks and --skip-agents keep generation hermetic: a git hook and an
	// AGENTS.md rewrite are effects on the workspace that have nothing to do
	// with its size, and the hook would then run on every write probe.
	if err := runQuiet(root, bin.path, "init", "--prefix", "bench", "--skip-hooks", "--skip-agents"); err != nil {
		return generatedStore{}, err
	}
	for i, batch := range importBatches(sz.rows) {
		specPath := filepath.Join(parent, fmt.Sprintf("%s-spec-%d.json", sz.name, i))
		blob, err := json.Marshal(batch)
		if err != nil {
			return generatedStore{}, fmt.Errorf("encoding import spec: %w", err)
		}
		if err := os.WriteFile(specPath, blob, 0o644); err != nil {
			return generatedStore{}, fmt.Errorf("writing import spec: %w", err)
		}
		if err := runQuiet(root, bin.path, "import", "--path", specPath); err != nil {
			return generatedStore{}, err
		}
	}
	info, err := workspace.Resolve(root)
	if err != nil {
		return generatedStore{}, fmt.Errorf("resolving generated workspace %s: %w", root, err)
	}
	return generatedStore{size: sz, root: root, databasePath: info.DatabasePath}, nil
}

// importBatches renders a row count as the import calls that produce it — one
// batch here, none at all for the empty store.
//
// It returns a list rather than a single spec because `lit import` refuses an
// empty file ("no issues in input"), so the empty store is not a smaller import
// but zero imports. Expressing that as a list length keeps it out of the caller
// as a branch: the loop over batches runs zero times, and the operation that
// runs is the same one at every size. [LAW:dataflow-not-control-flow]
func importBatches(rows int) [][]importRecord {
	records := make([]importRecord, 0, rows)
	for i := range rows {
		rec := importRecord{
			LocalID:     fmt.Sprintf("r%d", i),
			Title:       pad(fmt.Sprintf("generated row %d ", i), titleBytes),
			Type:        "task",
			Topic:       "bench",
			Description: pad(fmt.Sprintf("generated description for row %d ", i), descriptionBytes),
		}
		// The first row has no predecessor to depend on, which is why the
		// blocked share is "roughly" a third rather than exactly one.
		if i > 0 && i%blockedEveryNth == 0 {
			rec.DependsOn = []string{fmt.Sprintf("r%d", i-1)}
		}
		records = append(records, rec)
	}
	if len(records) == 0 {
		return nil
	}
	return [][]importRecord{records}
}

// pad repeats seed until it is exactly n bytes. Repeating real words rather
// than padding with one character keeps the row compressible the way prose is:
// a run of identical bytes would compress to nothing in the store's chunker and
// understate every byte figure this tool reports.
func pad(seed string, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(seed)
	}
	return b.String()[:n]
}

// storeBytes totals the store directory on disk.
//
// This is data bytes: a generated store has no git remote, so it carries none
// of the mirror's cached remote clone. That is worth stating because it makes
// the figure here much smaller than `du` on a live workspace, and the
// difference is not overhead — measured on this repository on 2026-09-21, a
// 279 MB store was 181 MB of .dolt/git-remote-cache against 83 MB of data. A
// size ceiling read off a live `du` would be budgeting mostly for a rebuildable
// cache.
//
// Apparent size, not blocks: the same store measured on two filesystems with
// different block sizes must produce the same number, or figures stop being
// comparable across machines.
func storeBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measuring %s: %w", dir, err)
	}
	return total, nil
}

// runQuiet runs a setup command, discarding its stdout and surfacing everything
// about a failure: the command, its exit status, and its combined output.
// Generation failures are the ones most likely to be misread — a store that
// half-imported still produces a plausible-looking table — so nothing here is
// allowed to continue past one. [LAW:no-silent-failure]
func runQuiet(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s (in %s): %w\n%s", name, strings.Join(args, " "), dir, err, out)
	}
	return nil
}
