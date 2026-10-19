// candybar-check renders lit's cc-candybar plugin on a pinned cc-candybar and
// fails when any of its segments shows other than a case expects. The plugin's
// `when` and `template` strings are Go templates that only cc-candybar
// evaluates: a broken one is still valid JSON5, so internal/statusline's tests
// cannot see it, and it would ship to every bar as an error cell or a hidden
// one. cc-candybar is a node package, so this is a CI job of its own (ci.yml's
// candybar job) rather than a Go test, and the inner loop needs neither node
// nor the network.
//
// Each case is a checkout as some lit leaves it. statusline.Write records the
// case's facts in the checkout's file, as every lit command does, and
// `cc-candybar plugin install` installs the definition this repository ships
// at .cc-candybar/plugins/lit/, from this checkout's working tree. Each render runs under a
// HOME and XDG dirs of its own, so nothing else on the machine reaches the bar.
//
// Every case prints one line on stdout, whatever its verdict, and the run ends
// with a summary:
//
//	candybar-check: case=phase verdict=ok cells=["🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1) 1/4 · review"] rows=[]
//	candybar-check: cc-candybar=1.136.0 plugin=/path/to/links cases=10 failed=0
//
// plugin= is the repository the plugin was installed from: the checkout the
// tool runs in.
//
// Exit codes: 0, every case rendered the cells and rows it expects; 1, a case
// did not, or the run could not start: no repository to install the plugin from, or
// cc-candybar could not be installed. A run that could not start prints no
// summary.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/statusline"
)

// candybarVersion is the cc-candybar every case renders on. Moving it is
// checking the plugin against another release: the cells it expects may move
// with it.
const candybarVersion = "1.136.0"

// layout makes lit's one segment the bar's whole tree. A tree at root
// replaces cc-candybar's own rows, so the bar holds cc-candybar's menu cell
// and lit's cell, and nothing else.
const layout = `{ root: { h: ['plugins.lit.ticket'] } }` + "\n"

// detailOpen is layout with the description dropped: `check` renders with no
// session state, so what ▸ opens is open only when the segment's state
// variable defaults so. The key is spelled as cc-candybar keeps the plugin's,
// under its namespace, so the ▾ this draws writes the key this reads.
const detailOpen = `{
  root: { h: ['plugins.lit.ticket'] },
  segments: { 'plugins.lit.ticket': { vars: { detail: { kind: 'state', key: 'plugins.lit.detail', default: 'open' } } } },
}` + "\n"

// descriptionOff is layout with the placement's description chars at 0.
const descriptionOff = `{ root: { h: [{ seg: 'plugins.lit.ticket', settings: { description: 0 } }] } }` + "\n"

var recordedAt = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)

// renderCase is one checkout's statusline file, the config it renders under
// (layout unless it says), the cells lit's segment shows for it — one, or
// none when it hides — and the rows it drops under the bar, each row's cells.
type renderCase struct {
	name   string
	write  func(statusline.Location) error
	config string
	cells  []string
	rows   [][]string
}

// rendered is what one render drew: the bar's cells after the menu, and the
// cells of each row under the bar.
type rendered struct {
	cells []string
	rows  [][]string
}

var (
	one       = statusline.Ticket{ID: "t-1", Title: "One"}
	two       = statusline.Ticket{ID: "t-2", Title: "Two"}
	described = statusline.Ticket{ID: "t-1", Title: "One", Description: "What one is.\nIn two lines."}
	lane      = statusline.RollupProgress{CompletedCount: 1, TotalCount: 4}
)

// The ▸ that drops the description writes the session's `detail` key, which
// cc-candybar keeps under the plugin's namespace; `check` renders as this one
// session. Open, the same place is the ▾ that folds it.
const (
	more = "[▸](cc-candybar://dispatch/e=set-state%2Ftest0a1b-2c3d-4e5f-6a7b-8c9d0e1f2a3b%2Fplugins.lit.detail%2Fopen)"
	less = "[▾](cc-candybar://dispatch/e=set-state%2Ftest0a1b-2c3d-4e5f-6a7b-8c9d0e1f2a3b%2Fplugins.lit.detail%2Fclosed)"
)

var cases = []renderCase{
	{
		name: "phase",
		write: litWrites(statusline.NewStarted(one, lane),
			statusline.PhaseRead{ID: one.ID, Phase: "review"}, statusline.Picked{Ticket: &two}),
		cells: []string{"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1) 1/4 · review · next [t-2](cc-candybar://dispatch/e=copy%2Ft-2)"},
	},
	{
		// A ticket with a description carries the ▸ that drops it; one
		// without (every other case) carries none.
		name: "description",
		write: litWrites(statusline.NewStarted(described, lane),
			statusline.PhaseRead{ID: one.ID, Phase: "review"}, statusline.Picked{Ticket: &two}),
		cells: []string{"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1) 1/4 · review · next [t-2](cc-candybar://dispatch/e=copy%2Ft-2) " + more},
	},
	{
		// Dropped, the description is one row under the bar, its lines
		// joined, and the ▸ is the ▾ that folds it.
		name:   "description-open",
		write:  litWrites(statusline.NewStarted(described, lane)),
		config: detailOpen,
		cells:  []string{"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1) 1/4 " + less},
		rows:   [][]string{{"What one is. In two lines."}},
	},
	{
		// description chars at 0 is the part turned off: no ▸, and nothing
		// dropped even with the state open.
		name:   "description-off",
		write:  litWrites(statusline.NewStarted(described, lane)),
		config: descriptionOff,
		cells:  []string{"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1) 1/4"},
	},
	{
		// No phase read: it is null. next names the ticket being worked, so it
		// says nothing ticket does not, and hides.
		name:  "null-phase",
		write: litWrites(statusline.NewStarted(one, lane), statusline.Picked{Ticket: &one}),
		cells: []string{"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1) 1/4"},
	},
	{
		name:  "no-phase-key",
		write: olderLitWrites(withoutKey("phase"), statusline.NewStarted(one, lane)),
		cells: []string{"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1) 1/4"},
	},
	{
		// lit 0.19 to 0.21 wrote null for a lane the recording command did
		// not count.
		name:  "null-rollup",
		write: olderLitWrites(nullKey("rollup_progress"), statusline.NewStarted(one, lane)),
		cells: []string{"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1)"},
	},
	{
		name:  "no-working-on",
		write: litWrites(statusline.Picked{Ticket: &two}),
		cells: []string{"next [t-2](cc-candybar://dispatch/e=copy%2Ft-2)"},
	},
	{
		name:  "nothing-ready",
		write: litWrites(statusline.NewStarted(one, lane), statusline.Picked{Ticket: nil}),
		cells: []string{"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1) 1/4 · next: nothing ready"},
	},
	{
		name:  "no-file",
		write: noFile,
		cells: []string{},
	},
}

// litWrites leaves the file this lit writes after recording facts.
func litWrites(facts ...statusline.Fact) func(statusline.Location) error {
	return func(loc statusline.Location) error {
		_, err := statusline.Write(loc, facts, recordedAt)
		return err
	}
}

// olderLitWrites leaves the file an older lit writes after recording facts:
// the file this lit writes, with older applied to its working_on.
func olderLitWrites(older func(workingOn map[string]json.RawMessage) error, facts ...statusline.Fact) func(statusline.Location) error {
	return func(loc statusline.Location) error {
		if err := litWrites(facts...)(loc); err != nil {
			return err
		}
		raw, err := os.ReadFile(loc.File)
		if err != nil {
			return err
		}
		// Raw messages keep every key this lit writes but the one older
		// changes, so the file tracks statusline.File without restating its
		// shape.
		var file, workingOn map[string]json.RawMessage
		if err := json.Unmarshal(raw, &file); err != nil {
			return err
		}
		if err := json.Unmarshal(file["working_on"], &workingOn); err != nil {
			return err
		}
		if err := older(workingOn); err != nil {
			return err
		}
		if file["working_on"], err = json.Marshal(workingOn); err != nil {
			return err
		}
		older, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(loc.File, older, 0o644)
	}
}

// withoutKey is a working_on from a lit before key: it has no such key at all.
func withoutKey(key string) func(map[string]json.RawMessage) error {
	return func(workingOn map[string]json.RawMessage) error {
		if _, ok := workingOn[key]; !ok {
			return fmt.Errorf("the facts recorded no working_on with a %s key to drop", key)
		}
		delete(workingOn, key)
		return nil
	}
}

// nullKey is a working_on from a lit that wrote key as null.
func nullKey(key string) func(map[string]json.RawMessage) error {
	return func(workingOn map[string]json.RawMessage) error {
		if _, ok := workingOn[key]; !ok {
			return fmt.Errorf("the facts recorded no working_on with a %s key to null", key)
		}
		workingOn[key] = json.RawMessage("null")
		return nil
	}
}

// noFile leaves the checkout as one no lit has recorded anything in.
func noFile(statusline.Location) error { return nil }

func main() {
	os.Exit(run(os.Stdout, os.Stderr))
}

func run(stdout, stderr io.Writer) int {
	dir, err := os.MkdirTemp("", "candybar-check-")
	if err != nil {
		fmt.Fprintf(stderr, "error: candybar-check: %v\n", err)
		return 1
	}
	defer os.RemoveAll(dir)
	// git's own stderr says why there is no repository.
	toplevel := exec.Command("git", "rev-parse", "--show-toplevel")
	toplevel.Stderr = stderr
	top, err := toplevel.Output()
	if err != nil {
		fmt.Fprintf(stderr, "error: candybar-check: find the repository to install the plugin from: %v\n", err)
		return 1
	}
	repo := strings.TrimSpace(string(top))
	candybar, err := install(dir, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "error: candybar-check: install cc-candybar %s: %v\n", candybarVersion, err)
		return 1
	}
	return check(stdout, stderr, repo, cases, func(c renderCase) (rendered, error) {
		return render(candybar, repo, filepath.Join(dir, c.name), c)
	})
}

// check renders every case, prints each one's verdict and then the summary
// naming the repository the plugin came from, and returns the exit code: 1
// when any case did not render its cells and rows. An absent rows is no rows,
// as an absent cells is none.
func check(stdout, stderr io.Writer, plugin string, cases []renderCase, render func(renderCase) (rendered, error)) int {
	failed := 0
	for _, c := range cases {
		got, err := render(c)
		verdict := "ok"
		switch {
		case err != nil:
			verdict = "error"
			fmt.Fprintf(stderr, "error: candybar-check: case %s: %v\n", c.name, err)
		case !slices.Equal(got.cells, c.cells) || !slices.EqualFunc(got.rows, c.rows, slices.Equal[[]string]):
			verdict = "mismatch"
			fmt.Fprintf(stderr, "error: candybar-check: case %s: cells %q rows %q, want %q %q\n", c.name, got.cells, got.rows, c.cells, c.rows)
		}
		if verdict != "ok" {
			failed++
		}
		fmt.Fprintf(stdout, "candybar-check: case=%s verdict=%s cells=%q rows=%q\n", c.name, verdict, got.cells, got.rows)
	}
	fmt.Fprintf(stdout, "candybar-check: cc-candybar=%s plugin=%s cases=%d failed=%d\n", candybarVersion, plugin, len(cases), failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// install puts cc-candybar under dir and returns the path of its command.
func install(dir string, stderr io.Writer) (string, error) {
	cmd := exec.Command("npm", "install", "--prefix", dir, "--no-audit", "--no-fund",
		"@promptctl/cc-candybar@"+candybarVersion)
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return filepath.Join(dir, "node_modules", ".bin", "cc-candybar"), nil
}

// render lays out one case under root, with the plugin installed from repo,
// and returns what `cc-candybar check --render` draws for it.
func render(candybar, repo, root string, c renderCase) (rendered, error) {
	none := rendered{}
	home := filepath.Join(root, "home")
	data := filepath.Join(root, "data")
	checkout := filepath.Join(root, "checkout")
	config := filepath.Join(root, "config.json5")
	if err := os.MkdirAll(home, 0o755); err != nil {
		return none, err
	}
	// PATH finds node, which cc-candybar's command runs under; nothing else
	// of this process's environment reaches the install or the render.
	env := []string{"PATH=" + os.Getenv("PATH"),
		"HOME=" + home, "XDG_DATA_HOME=" + data, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config")}
	plugin := exec.Command(candybar, "plugin", "install", repo)
	plugin.Env = env
	if out, err := plugin.CombinedOutput(); err != nil {
		return none, fmt.Errorf("cc-candybar plugin install: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "init", "-q", checkout).CombinedOutput(); err != nil {
		return none, fmt.Errorf("git init: %v: %s", err, out)
	}
	if err := c.write(statusline.LocationOf(checkout, filepath.Join(checkout, ".git"))); err != nil {
		return none, fmt.Errorf("write statusline file: %w", err)
	}
	text := c.config
	if text == "" {
		text = layout
	}
	if err := os.WriteFile(config, []byte(text), 0o644); err != nil {
		return none, err
	}
	var stdout, stderr strings.Builder
	cmd := exec.Command(candybar, "check", "--render", config)
	cmd.Dir = checkout
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return none, fmt.Errorf("cc-candybar check --render: %v\n%s", err, stderr.String())
	}
	return cells(stdout.String())
}

var (
	// sgr is a Select Graphic Rendition sequence: colour and weight, which a
	// cell's text does not depend on.
	sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")
	// hyperlink is an OSC 8 hyperlink, the form a cell's click action takes.
	hyperlink = regexp.MustCompile("\x1b\\]8;[^;\x1b]*;([^\x1b]*)\x1b\\\\([^\x1b]*)\x1b\\]8;;\x1b\\\\")
	// separator is a powerline arrow between cells: the solid one where the
	// colour changes, the thin one between two cells of one colour.
	separator = regexp.MustCompile("[\ue0b0\ue0b1]")
)

// menuCell opens every bar cc-candybar draws: its settings menu, which no
// config can remove.
const menuCell = "🍫"

// cells reads what `check --render` prints — its config OK line, the bar,
// then any row a segment dropped under the bar — as the bar's cells after the
// menu and each row's cells, each a cell's text with any click action written
// [text](url).
func cells(printed string) (rendered, error) {
	none := rendered{}
	lines := strings.Split(strings.TrimSuffix(printed, "\n"), "\n")
	if len(lines) < 2 || !strings.HasSuffix(lines[0], ": config OK") {
		return none, fmt.Errorf("check --render printed %q, want a config OK line and a bar line", printed)
	}
	bar, err := lineCells(lines[1])
	if err != nil {
		return none, err
	}
	if !strings.HasPrefix(bar[0], "["+menuCell+"](") {
		return none, fmt.Errorf("bar %q does not open with cc-candybar's %s menu cell", lines[1], menuCell)
	}
	got := rendered{cells: bar[1:], rows: [][]string{}}
	for _, line := range lines[2:] {
		row, err := lineCells(line)
		if err != nil {
			return none, err
		}
		got.rows = append(got.rows, row)
	}
	return got, nil
}

// lineCells reads one drawn line as its cells.
func lineCells(line string) ([]string, error) {
	text := hyperlink.ReplaceAllString(sgr.ReplaceAllString(line, ""), "[$2]($1)")
	if strings.Contains(text, "\x1b") {
		return nil, fmt.Errorf("line holds an escape sequence this check does not read: %q", text)
	}
	// A line opens and closes on an arrow, so the text before the first and
	// after the last is empty; every piece between is a drawn cell, and one
	// whose template rendered only spaces is kept, blank, not read as hidden.
	pieces := separator.Split(text, -1)
	if len(pieces) < 3 || strings.TrimSpace(pieces[0]) != "" || strings.TrimSpace(pieces[len(pieces)-1]) != "" {
		return nil, fmt.Errorf("line %q does not open and close on an arrow", text)
	}
	drawn := []string{}
	for _, cell := range pieces[1 : len(pieces)-1] {
		drawn = append(drawn, strings.TrimSpace(cell))
	}
	return drawn, nil
}
