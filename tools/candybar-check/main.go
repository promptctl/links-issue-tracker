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
// at .cc-candybar/plugins/lit/, as a user installs it. Each render runs under a
// HOME and XDG dirs of its own, so nothing else on the machine reaches the bar.
//
// Every case prints one line on stdout, whatever its verdict, and the run ends
// with a summary:
//
//	candybar-check: case=phase verdict=ok cells=["phase review"]
//	candybar-check: cc-candybar=1.130.5 cases=6 failed=0
//
// Exit codes: 0, every case rendered the cells it expects; 1, a case did not,
// or cc-candybar could not be installed.
package main

import (
	"encoding/json"
	"errors"
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
const candybarVersion = "1.130.5"

// layout makes lit's three segments the bar's whole tree, in the order cells
// are expected. A tree at root replaces cc-candybar's own rows, so the bar
// holds cc-candybar's menu cell and lit's cells, and nothing else.
const layout = `{ root: { h: ['plugins.lit.ticket', 'plugins.lit.phase', 'plugins.lit.next'] } }` + "\n"

var recordedAt = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)

// renderCase is one checkout's statusline file and the cells lit's segments
// show for it, in layout order. A segment that hides shows no cell.
type renderCase struct {
	name  string
	write func(statusline.Location) error
	cells []string
}

var (
	one  = statusline.Ticket{ID: "t-1", Title: "One"}
	two  = statusline.Ticket{ID: "t-2", Title: "Two"}
	lane = statusline.RollupProgress{CompletedCount: 1, TotalCount: 4}
)

var cases = []renderCase{
	{
		name: "phase",
		write: litWrites(statusline.Started{Ticket: one}, statusline.RollupCounted{ID: one.ID, Progress: lane},
			statusline.PhaseRead{ID: one.ID, Phase: "review"}, statusline.Picked{Ticket: &two}),
		cells: []string{
			"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1) 1/4",
			"phase review",
			"next [t-2](cc-candybar://dispatch/e=copy%2Ft-2)",
		},
	},
	{
		// No phase read and no lane counted: both are null. next names the
		// ticket being worked, so it says nothing ticket does not, and hides.
		name:  "null-phase",
		write: litWrites(statusline.Started{Ticket: one}, statusline.Picked{Ticket: &one}),
		cells: []string{"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1)"},
	},
	{
		name:  "no-phase-key",
		write: olderLitWrites(statusline.Started{Ticket: one}, statusline.RollupCounted{ID: one.ID, Progress: lane}),
		cells: []string{"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1) 1/4"},
	},
	{
		name:  "no-working-on",
		write: litWrites(statusline.Picked{Ticket: &two}),
		cells: []string{"next [t-2](cc-candybar://dispatch/e=copy%2Ft-2)"},
	},
	{
		name:  "nothing-ready",
		write: litWrites(statusline.Started{Ticket: one}, statusline.Picked{Ticket: nil}),
		cells: []string{"🎫 [t-1](cc-candybar://dispatch/e=copy%2Ft-1)", "next nothing ready"},
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

// olderLitWrites leaves the file a lit from before phases writes after
// recording facts: its working_on has no phase key at all.
func olderLitWrites(facts ...statusline.Fact) func(statusline.Location) error {
	return func(loc statusline.Location) error {
		if err := litWrites(facts...)(loc); err != nil {
			return err
		}
		raw, err := os.ReadFile(loc.File)
		if err != nil {
			return err
		}
		// Raw messages keep every key this lit writes but the one dropped, so
		// the file tracks statusline.File without restating its shape.
		var file, workingOn map[string]json.RawMessage
		if err := json.Unmarshal(raw, &file); err != nil {
			return err
		}
		if err := json.Unmarshal(file["working_on"], &workingOn); err != nil {
			return err
		}
		if _, ok := workingOn["phase"]; !ok {
			return errors.New("the facts recorded no working_on with a phase key to drop")
		}
		delete(workingOn, "phase")
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
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
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
	return check(stdout, stderr, cases, func(c renderCase) ([]string, error) {
		return render(candybar, repo, filepath.Join(dir, c.name), c)
	})
}

// check renders every case, prints each one's verdict and then the summary,
// and returns the exit code: 1 when any case did not render its cells.
func check(stdout, stderr io.Writer, cases []renderCase, render func(renderCase) ([]string, error)) int {
	failed := 0
	for _, c := range cases {
		got, err := render(c)
		verdict := "ok"
		switch {
		case err != nil:
			verdict = "error"
			fmt.Fprintf(stderr, "error: candybar-check: case %s: %v\n", c.name, err)
		case !slices.Equal(got, c.cells):
			verdict = "mismatch"
			fmt.Fprintf(stderr, "error: candybar-check: case %s: cells %q, want %q\n", c.name, got, c.cells)
		}
		if verdict != "ok" {
			failed++
		}
		fmt.Fprintf(stdout, "candybar-check: case=%s verdict=%s cells=%q\n", c.name, verdict, got)
	}
	fmt.Fprintf(stdout, "candybar-check: cc-candybar=%s cases=%d failed=%d\n", candybarVersion, len(cases), failed)
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
// and returns the cells `cc-candybar check --render` draws for it.
func render(candybar, repo, root string, c renderCase) ([]string, error) {
	home := filepath.Join(root, "home")
	data := filepath.Join(root, "data")
	checkout := filepath.Join(root, "checkout")
	config := filepath.Join(root, "config.json5")
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, err
	}
	// PATH finds node, which cc-candybar's command runs under; nothing else
	// of this process's environment reaches the install or the render.
	env := []string{"PATH=" + os.Getenv("PATH"),
		"HOME=" + home, "XDG_DATA_HOME=" + data, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config")}
	plugin := exec.Command(candybar, "plugin", "install", repo)
	plugin.Env = env
	if out, err := plugin.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("cc-candybar plugin install: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "init", "-q", checkout).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git init: %v: %s", err, out)
	}
	if err := c.write(statusline.LocationOf(checkout, filepath.Join(checkout, ".git"))); err != nil {
		return nil, fmt.Errorf("write statusline file: %w", err)
	}
	if err := os.WriteFile(config, []byte(layout), 0o644); err != nil {
		return nil, err
	}
	var stdout, stderr strings.Builder
	cmd := exec.Command(candybar, "check", "--render", config)
	cmd.Dir = checkout
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("cc-candybar check --render: %v\n%s", err, stderr.String())
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

// cells reads what `check --render` prints, its config OK line and then the
// bar, as the bar's cells after the menu, each a cell's text with any click
// action written [text](url).
func cells(printed string) ([]string, error) {
	lines := strings.Split(strings.TrimSuffix(printed, "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], ": config OK") {
		return nil, fmt.Errorf("check --render printed %q, want a config OK line and one bar line", printed)
	}
	bar := hyperlink.ReplaceAllString(sgr.ReplaceAllString(lines[1], ""), "[$2]($1)")
	if strings.Contains(bar, "\x1b") {
		return nil, fmt.Errorf("bar holds an escape sequence this check does not read: %q", bar)
	}
	// The bar opens and closes on an arrow, so the text before the first and
	// after the last is empty; every piece between is a drawn cell, and one
	// whose template rendered only spaces is kept, blank, not read as hidden.
	pieces := separator.Split(bar, -1)
	if len(pieces) < 3 || strings.TrimSpace(pieces[0]) != "" || strings.TrimSpace(pieces[len(pieces)-1]) != "" {
		return nil, fmt.Errorf("bar %q does not open and close on an arrow", bar)
	}
	drawn := []string{}
	for _, cell := range pieces[1 : len(pieces)-1] {
		drawn = append(drawn, strings.TrimSpace(cell))
	}
	if !strings.HasPrefix(drawn[0], "["+menuCell+"](") {
		return nil, fmt.Errorf("bar %q does not open with cc-candybar's %s menu cell", bar, menuCell)
	}
	return drawn[1:], nil
}
