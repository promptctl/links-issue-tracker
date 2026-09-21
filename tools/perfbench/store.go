package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
	// os.Mkdir, never MkdirAll: an existing directory must fail here rather
	// than be reused. lit init is idempotent and lit import appends, so
	// generating into a populated workspace silently produces a store of 2N
	// rows wearing an N-row label — the tool's own answer-shaped void, in the
	// one place nothing downstream could detect it. Two ordinary invocations
	// reach it: `--keep <dir>` pointed a second time at a directory that still
	// holds the first run's stores, and any repeated size, which parseSizes
	// rejects for the same reason. CONTRIBUTING states the fresh-directory
	// requirement where it documents the flag, so the two agree rather than
	// each describing a different tool. [LAW:no-silent-failure]
	if err := os.Mkdir(root, 0o755); err != nil {
		return generatedStore{}, fmt.Errorf("creating workspace dir: %w "+
			"(a store is generated into a fresh directory; remove it or pass a different --keep)", err)
	}
	// lit resolves its store relative to a git checkout, so the workspace has
	// to be one. The identity is passed per-command rather than written into a
	// config file so generation cannot depend on — or disturb — whatever git
	// identity the machine running this has.
	// Every setting that could vary by machine is forced, because generation
	// that works here and fails on a colleague's box is a benchmark nobody can
	// reproduce. --template= keeps the machine's init templates (and their
	// hooks) out of the new repository; commit.gpgsign=false stops a developer
	// with global signing turned on from aborting the run on a key prompt;
	// --no-verify keeps a global core.hooksPath from running hooks on a
	// workspace that passed --skip-hooks to lit init for exactly that reason.
	steps := [][]string{
		{"git", "init", "-q", "--template=", "."},
		{"git", "-c", "user.email=perfbench@invalid", "-c", "user.name=perfbench",
			"-c", "commit.gpgsign=false",
			"commit", "-q", "--no-verify", "--allow-empty", "-m", "perfbench workspace"},
	}
	for _, step := range steps {
		if err := runQuiet(root, runBudget, step[0], step[1:]...); err != nil {
			return generatedStore{}, err
		}
	}
	// --skip-hooks and --skip-agents keep generation hermetic: a git hook and an
	// AGENTS.md rewrite are effects on the workspace that have nothing to do
	// with its size, and the hook would then run on every write probe.
	if err := runQuiet(root, runBudget, bin.path, "init", "--prefix", "bench", "--skip-hooks", "--skip-agents"); err != nil {
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
		if err := runQuiet(root, runBudget, bin.path, "import", "--path", specPath); err != nil {
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
			Title:       fmt.Sprintf("generated row %d %s", i, filler(i, titleBytes))[:titleBytes],
			Type:        "task",
			Topic:       "bench",
			Description: filler(i, descriptionBytes),
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

// proseWords is the vocabulary filler text is drawn from. Its content is
// irrelevant; its job is to make generated rows compress like the rows a real
// store holds.
var proseWords = strings.Fields(
	"the store opens a workspace and reads every row before printing which is why gather cost " +
		"matters more than expected when a backlog grows past several hundred tickets in one repository " +
		"measured against the envelope a reader loads one level of the graph per query so the planner " +
		"never merges ranges pairwise and the commit lock stops serializing every writer on contention")

// idTokenPercent is how often the filler emits a high-entropy token instead of
// a word. Real ticket prose is dense with issue ids, commit shas, paths and
// figures, and those are what carry its entropy.
const idTokenPercent = 40

// filler builds n bytes of text for row seed, deterministically.
//
// WHY NOT A REPEATED PHRASE. This function used to repeat one phrase until it
// reached the length, on the theory that repeating words rather than a single
// character kept the row compressible "the way prose is". Measured, that was
// wrong by an order of magnitude: against this repository's own 588 ticket
// descriptions, real prose truncated to 1310 bytes gzips 1.79x (median; the
// spread is 1.53x to 2.56x), while the repeated phrase gzipped 20.15x. Since
// the store compresses what it holds, a row 11x more compressible than a real
// one is simply not the row being modelled. The mix above is calibrated to that
// measurement rather than chosen: it lands at 1.83x.
//
// How much this moves the reported bytes is a separate question, and the answer
// is: less than the measurement can resolve. 590 descriptions are 0.77 MB of a
// ~23.5 MB store, so their compressibility bounds about 3% of the total, and
// the 590-row figure varies between 23.1 and 23.8 MB across runs regardless.
// This is therefore a FIDELITY fix — the fixture is now the thing it claims to
// model — and not a correction to the numbers. Do not quote a before/after
// across this change: the difference sits inside the run-to-run spread, and the
// honest statement is the 3% bound, which is also why a size ceiling belongs on
// structure rather than on what users type.
//
// Deterministic, so two runs of the same size generate byte-identical rows and
// a change in reported store bytes is a change in lit, never in the fixture.
func filler(seed int, n int) string {
	// A counter run through splitmix32's finalizer, spelled out rather than
	// taken from math/rand, so the bytes this produces are fixed by this source
	// and cannot shift under a change to the standard library's generator — a
	// benchmark fixture that changes with the toolchain would retroactively
	// invalidate every figure recorded against it. [LAW:one-source-of-truth]
	//
	// It is a mixed counter and not the plain LCG it replaces, because an LCG's
	// LOW bits are barely random and this function reads them twice per token.
	// With an odd multiplier and an odd increment, bit 0 of x*1664525+1013904223
	// strictly alternates; each token consumed exactly two draws, so the draw
	// that picks a word always landed on the same parity — and since
	// len(proseWords) is even, n%len inherits n's parity. Every row drew from
	// half the vocabulary, the half decided by its seed. The finalizer below
	// avalanches, so every bit of the output depends on every bit of the
	// counter and a draw modulo anything sees the whole range.
	x := uint32(seed)*2654435761 + 12345
	next := func() uint32 {
		x += 0x9e3779b9
		z := x
		z ^= z >> 16
		z *= 0x21f0aaad
		z ^= z >> 15
		z *= 0x735a2d97
		z ^= z >> 15
		return z
	}
	var b strings.Builder
	for b.Len() < n {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		if next()%100 < idTokenPercent {
			fmt.Fprintf(&b, "%08x", next())
			continue
		}
		b.WriteString(proseWords[next()%uint32(len(proseWords))])
	}
	return b.String()[:n]
}

// storeBytes totals the store directory on disk.
//
// This is data bytes: a generated store has no git remote, so it carries none
// of the mirror's cached remote clone. That is worth stating because it makes
// the figure here much smaller than a live workspace's, and the difference is
// not overhead — measured on this repository on 2026-09-21, a 291.5 MB store
// was 192.0 MB of .dolt/git-remote-cache against 99.5 MB of data. A size
// ceiling read off the whole directory would be budgeting mostly for a
// rebuildable cache.
//
// Quote those three figures together or not at all. They are apparent bytes,
// the unit this function returns; `du` reports allocated blocks and gives a
// different total for the same store, so a cache share taken from one and a
// total taken from the other do not describe one measurement.
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
//
// It carries the same runBudget the probes do, and for a stronger reason.
// runBudget exists because this epic's subject is a write path that can wait
// fifteen minutes on a lock; generation is ENTIRELY write path, so an import
// wedged behind a lock is at least as likely here as in any probe. Unbudgeted,
// that hangs the tool with nothing on screen — the exact outcome the budget was
// added to prevent.
func runQuiet(dir string, budget time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("%s %s (in %s) exceeded the %s budget and was killed:\n%s",
			name, strings.Join(args, " "), dir, budget, out)
	}
	if err != nil {
		return fmt.Errorf("%s %s (in %s): %w\n%s", name, strings.Join(args, " "), dir, err, out)
	}
	return nil
}
