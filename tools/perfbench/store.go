package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/environ"
	"github.com/promptctl/links-issue-tracker/internal/storage"
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
	// fleetRoot is the directory this store and every store generated beside
	// it live in: what the fleet probes name, and for a single store a root
	// holding that store alone.
	fleetRoot string
	// env is the environment every lit invocation against this store runs
	// under — generation and probes alike, so the store is measured by the
	// same lit that produced it. See hermeticEnv for what it excludes.
	env []string
	// ids are the issue ids lit minted for the generated rows, in the order
	// importBatches wrote them: row i is ids[i]. Empty for the empty store.
	ids []string
}

// generate builds every store of a scale point under parent/<name>, the
// measured store first: each distinct row count once, through lit, and every
// further store of that count as a byte copy of the first.
//
// WHY COPIES. The generator is deterministic in content (filler is seeded by
// row), so generating one count N times would produce the same rows N times at
// N times the import cost: a ten-times fleet is 300 stores, and import cost
// grows faster than its rows (20s at 700 rows, 146s at 2,800, measured
// 2026-10-23). A copy is the generator's output, replicated; each copy is then
// an independent store, written to and counted on its own.
//
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
func generate(bin litBinary, parent string, sz size, env []string, run stepRunner) ([]generatedStore, error) {
	root := filepath.Join(parent, sz.name)
	if err := os.Mkdir(root, 0o755); err != nil {
		return nil, fmt.Errorf("creating workspace dir: %w "+
			"(a store is generated into a fresh directory; remove it or pass a different --keep)", err)
	}
	first := map[int]generatedStore{}
	members := make([]generatedStore, 0, 1+len(sz.beside))
	for i, rows := range sz.members() {
		dir := filepath.Join(root, fmt.Sprintf("store-%03d", i))
		m, generated := first[rows]
		var err error
		if generated {
			m, err = replicate(m, dir)
		} else {
			m, err = generateStore(bin, dir, sz, rows, env, run)
			first[rows] = m
		}
		if err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, nil
}

// generateStore builds one workspace of rows rows at root, for the scale
// point sz.
//
// WHY IT SHELLS OUT INSTEAD OF CALLING internal/store. Seeding through the
// store API would be faster, and it would also be a second definition of what a
// populated lit store looks like — one that drifts from the real write path the
// moment a migration, a default, or a rank rule changes, and drifts silently,
// because nothing would compare the two. `lit init` plus `lit import` is the
// bulk-ingest path lit already offers its users, so the store measured here is
// a store lit itself produced. [LAW:one-source-of-truth]
// Each step runs through run, which is runQuiet everywhere but the test that
// proves lit accepts the plan, where it is lit's own entry point in-process.
func generateStore(bin litBinary, root string, sz size, rows int, env []string, run stepRunner) (generatedStore, error) {
	if err := os.Mkdir(root, 0o755); err != nil {
		return generatedStore{}, fmt.Errorf("creating workspace dir: %w", err)
	}
	specs := make([]importSpec, 0, 1)
	for i, batch := range importBatches(rows) {
		specPath := fmt.Sprintf("%s-spec-%d.json", root, i)
		blob, err := json.Marshal(batch)
		if err != nil {
			return generatedStore{}, fmt.Errorf("encoding import spec: %w", err)
		}
		if err := os.WriteFile(specPath, blob, 0o644); err != nil {
			return generatedStore{}, fmt.Errorf("writing import spec: %w", err)
		}
		specs = append(specs, importSpec{path: specPath, records: batch})
	}
	var ids []string
	for _, st := range generationSteps(bin, rows, specs) {
		out, err := run(root, env, st)
		if err != nil {
			return generatedStore{}, err
		}
		loaded, err := idsLoaded(out, st.loads)
		if err != nil {
			return generatedStore{}, fmt.Errorf("%s (in %s): %w", strings.Join(st.argv, " "), root, err)
		}
		ids = append(ids, loaded...)
	}
	return resolved(generatedStore{size: sz, root: root, fleetRoot: filepath.Dir(root), env: env, ids: ids})
}

// replicate copies src's workspace to dir and answers the copy, the same rows
// under the same ids in a store of its own.
func replicate(src generatedStore, dir string) (generatedStore, error) {
	if err := os.CopyFS(dir, os.DirFS(src.root)); err != nil {
		return generatedStore{}, fmt.Errorf("copying workspace %s to %s: %w", src.root, dir, err)
	}
	src.root = dir
	return resolved(src)
}

// resolved fills in where store's database lives. It comes from
// workspace.Resolve, the package that owns lit's path geometry, rather than
// from the workspace it was copied from, so a copy reports its own bytes.
func resolved(store generatedStore) (generatedStore, error) {
	info, err := workspace.Resolve(environ.Of(store.env), store.root)
	if err != nil {
		return generatedStore{}, fmt.Errorf("resolving generated workspace %s: %w", store.root, err)
	}
	store.databasePath = info.DatabasePath
	return store, nil
}

// stepRunner performs one generation step in dir under env, answering the
// step's stdout.
type stepRunner func(dir string, env []string, st step) ([]byte, error)

// step is one command generation runs, with the budget that bounds it and the
// records it loads into the store, if it is an import.
type step struct {
	budget time.Duration
	argv   []string
	// loads is the records this step creates, whose ids its stdout reports.
	// Every step has it, empty for all but an import, so reading ids back is
	// one operation run after every step rather than a branch on which step
	// just ran. [LAW:dataflow-not-control-flow]
	loads []storage.ImportTreeSpec
}

// importSpec is one import batch: the file `lit import` reads, and the records
// written into it.
type importSpec struct {
	path    string
	records []storage.ImportTreeSpec
}

// idsLoaded reads the issue ids lit minted for loads off an import's stdout,
// in the order of loads.
//
// The source is the mapping `lit import` documents and prints, one
// `  <local_id> -> <id>` line per record it created, and not a `lit ls`
// afterwards: the import that made the ids is the one authority on which id
// each record got, and a second read would be a second source with its own
// failure modes, run before any probe could name a slow command.
// [LAW:one-source-of-truth] The order is the spec's, mapped through each
// record's local id, because lit creates and prints in dependency order.
// A record the output does not map, or a local id it maps twice, fails
// loudly: a store missing an id it was built with would quietly drop the
// probes that name it. [LAW:no-silent-failure] Only lines naming one of
// loads' local ids are read, so a step that loads nothing reads nothing,
// whatever its output happens to contain.
func idsLoaded(stdout []byte, loads []storage.ImportTreeSpec) ([]string, error) {
	wanted := map[string]bool{}
	for _, rec := range loads {
		wanted[rec.LocalID] = true
	}
	minted := map[string]string{}
	for _, line := range strings.Split(string(stdout), "\n") {
		local, id, found := strings.Cut(strings.TrimSpace(line), " -> ")
		if !found || !wanted[local] {
			continue
		}
		if _, dup := minted[local]; dup {
			return nil, fmt.Errorf("import output maps local id %q more than once:\n%s", local, stdout)
		}
		minted[local] = id
	}
	ids := make([]string, 0, len(loads))
	for _, rec := range loads {
		id, ok := minted[rec.LocalID]
		if !ok {
			return nil, fmt.Errorf("import output does not map local id %q:\n%s", rec.LocalID, stdout)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// generationSteps is the plan that turns an empty directory into a workspace
// of rows rows: the commands, in order, each with its budget. It is a pure
// function of its inputs so the plan can be checked without running it —
// which is how the one step whose budget scales with the row count is pinned.
// [LAW:effects-at-boundaries]
//
// lit resolves its store relative to a git checkout, so the workspace has to
// be one. The identity is passed per-command rather than written into a
// config file so generation cannot depend on — or disturb — whatever git
// identity the machine running this has. Every setting that could vary by
// machine is forced, because generation that works here and fails on a
// colleague's box is a benchmark nobody can reproduce. --template= keeps the
// machine's init templates (and their hooks) out of the new repository;
// commit.gpgsign=false stops a developer with global signing turned on from
// aborting the run on a key prompt; --no-verify keeps a global core.hooksPath
// from running hooks on a workspace that passed --skip-hooks to lit init for
// exactly that reason. --skip-hooks and --skip-agents keep generation
// hermetic: a git hook and an AGENTS.md rewrite are effects on the workspace
// that have nothing to do with its size, and the hook would then run on every
// write probe.
//
// The import carries importBudget rather than runBudget because its length
// grows with the rows it loads; every other step is the same size at every
// size.
func generationSteps(bin litBinary, rows int, specs []importSpec) []step {
	steps := []step{
		{budget: runBudget, argv: []string{"git", "init", "-q", "--template=", "."}},
		{budget: runBudget, argv: []string{"git", "-c", "user.email=perfbench@invalid", "-c", "user.name=perfbench",
			"-c", "commit.gpgsign=false",
			"commit", "-q", "--no-verify", "--allow-empty", "-m", "perfbench workspace"}},
		{budget: runBudget, argv: []string{bin.path, "init", "--prefix", "bench", "--skip-hooks", "--skip-agents"}},
	}
	for _, spec := range specs {
		steps = append(steps, step{budget: importBudget(rows), argv: []string{bin.path, "import", "--path", spec.path}, loads: spec.records})
	}
	return steps
}

// importBatches renders a row count as the import calls that produce it — one
// batch here, none at all for the empty store.
//
// It returns a list rather than a single spec because `lit import` refuses an
// empty file ("no issues in input"), so the empty store is not a smaller import
// but zero imports. Expressing that as a list length keeps it out of the caller
// as a branch: the loop over batches runs zero times, and the operation that
// runs is the same one at every size. [LAW:dataflow-not-control-flow]
//
// Each row is a storage.ImportTreeSpec, the record `lit import` parses, so the
// spec this tool writes cannot drift from the one lit reads.
// [LAW:one-source-of-truth]
//
// The rows come in groups of rowsPerEpic: an epic, then the tasks inside it,
// because lit refuses a ticket outside an epic. A dependency joins a task to
// the task at the same position in the previous group — an edge between two
// epics' children, the form lit accepts; lit refuses a blocked_by between two
// children of one epic, whose order is their rank.
func importBatches(rows int) [][]storage.ImportTreeSpec {
	records := make([]storage.ImportTreeSpec, 0, rows)
	for i := range rows {
		// The title draws from its own stream (^i, distinct from every
		// description's seed i >= 0): on a shared seed the title would be the
		// first 50-odd bytes of its description verbatim, and a store that
		// compresses the two together would store the row cheaper than a real
		// one whose title is independent prose.
		rec := storage.ImportTreeSpec{
			LocalID:     localID(i),
			Title:       fmt.Sprintf("generated row %d %s", i, filler(^i, titleBytes))[:titleBytes],
			IssueType:   "epic",
			Topic:       "bench",
			Description: filler(i, descriptionBytes),
		}
		if i%rowsPerEpic != 0 {
			rec.IssueType = "task"
			rec.Parent = localID(i - i%rowsPerEpic)
			// The first group has no group before it to depend on, which is
			// why the blocked share is "roughly" a third rather than exactly.
			if i >= rowsPerEpic && i%blockedEveryNth == 0 {
				rec.BlockedBy = []string{localID(i - rowsPerEpic)}
			}
		}
		records = append(records, rec)
	}
	if len(records) == 0 {
		return nil
	}
	return [][]storage.ImportTreeSpec{records}
}

// localID is the name row i goes by inside its import spec.
func localID(i int) string { return fmt.Sprintf("r%d", i) }

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
// WHY NOT A REPEATED PHRASE. Against this repository's own 588 ticket
// descriptions, real prose truncated to 1310 bytes gzips 1.79x (median; the
// spread is 1.53x to 2.56x), while a repeated phrase gzips 20.15x. Since the
// store compresses what it holds, a row 11x more compressible than a real one
// is simply not the row being modelled. The mix above is calibrated to that
// measurement rather than chosen: it lands at 1.83x.
//
// How much this moves the reported bytes is less than the measurement can
// resolve. 590 descriptions are 0.77 MB of a ~23.5 MB store, so their
// compressibility bounds about 3% of the total, and the 590-row figure varies
// between 23.1 and 23.8 MB across runs regardless. The filler is a FIDELITY
// measure — the fixture is the thing it claims to model — not a lever on the
// numbers, and that 3% bound is also why a size ceiling belongs on structure
// rather than on what users type.
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
	// It is a mixed counter and not a plain LCG, because an LCG's LOW bits are
	// barely random and this function reads them twice per token. With an odd
	// multiplier and an odd increment, bit 0 of x*1664525+1013904223 strictly
	// alternates; with two draws per token, the draw that picks a word would
	// always land on the same parity — and with an even len(proseWords), n%len
	// inherits n's parity, so every row would draw from half the vocabulary,
	// the half decided by its seed. The finalizer below avalanches, so every bit
	// of the output depends on every bit of the counter and a draw modulo
	// anything sees the whole range.
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

// runQuiet runs one generation step, answering its stdout (the one thing a
// caller reads from a step, an import's id mapping) and surfacing everything
// about a failure: the command, its exit status, and both of its streams,
// each labeled, since they are captured apart. It takes the step whole so the
// budget arrives with the argv it was planned for, rather than as a second
// argument a caller could pair wrongly.
// Generation failures are the ones most likely to be misread — a store that
// half-imported still produces a plausible-looking table — so nothing here is
// allowed to continue past one. [LAW:no-silent-failure]
//
// Every step carries a budget because this epic's subject is a write path
// that can wait fifteen minutes on a lock; generation is ENTIRELY write path,
// so an import wedged behind a lock is at least as likely here as anywhere.
// Unbudgeted, that hangs the tool with nothing on screen.
func runQuiet(dir string, env []string, st step) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), st.budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, st.argv[0], st.argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = killGrace
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s (in %s) exceeded the %s budget and was killed:\n%s",
			strings.Join(st.argv, " "), dir, st.budget, streams(stdout.Bytes(), stderr.Bytes()))
	}
	if err != nil {
		return nil, fmt.Errorf("%s (in %s): %w\n%s", strings.Join(st.argv, " "), dir, err, streams(stdout.Bytes(), stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

// streams prints a failed step's two outputs under their names.
func streams(stdout, stderr []byte) string {
	return fmt.Sprintf("--- stdout ---\n%s\n--- stderr ---\n%s", stdout, stderr)
}
