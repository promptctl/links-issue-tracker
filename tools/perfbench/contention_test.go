package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The storm's verdict is every writer that did not complete, named by number,
// in order, with the error that says what it did instead — and every writer
// runs whatever the others did, so a failure early in the storm cannot hide
// the ones after it.
func TestStormNamesEveryWriterThatDidNotComplete(t *testing.T) {
	store := generatedStore{size: size{name: "one", rows: 1}, root: t.TempDir()}
	var (
		mu  sync.Mutex
		ran []string
	)
	run := func(dir string, env []string, st step) ([]byte, error) {
		title := st.argv[slices.Index(st.argv, "--title")+1]
		mu.Lock()
		ran = append(ran, title)
		mu.Unlock()
		if title == "perfbench storm writer 3" || title == "perfbench storm writer 77" {
			return nil, errors.New("workspace_busy: held by a process that left no record")
		}
		return nil, nil
	}

	failed := storm(litBinary{path: "lit"}, store, run)

	if len(ran) != stormWriters {
		t.Fatalf("the storm ran %d writers, want all %d", len(ran), stormWriters)
	}
	var writers []int
	for _, f := range failed {
		writers = append(writers, f.writer)
	}
	if !slices.Equal(writers, []int{3, 77}) {
		t.Fatalf("failed writers = %v, want [3 77]", writers)
	}
	if got := failed[0].String(); !strings.Contains(got, "store one (1 rows): writer 3 of 100 did not complete: workspace_busy") {
		t.Errorf("verdict %q does not name the store, the writer and its error", got)
	}
}

// A failed writer reaches progress the moment the storm ends and comes back as
// the store's verdict, which is what run exits on; a writer that completed
// says nothing. The fake lit does just enough to be generated and measured,
// and refuses exactly one storm writer.
func TestAStoresFailedWritersAreLoggedAndReturned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "lit")
	script := `#!/bin/sh
case "$1" in
  init) mkdir -p .git/links/dolt ;;
  import) printf 'imported 1 issues\n  r0 -> bench-x\n' ;;
  new) [ "$3" = "perfbench storm writer 7" ] && { echo "workspace busy" >&2; exit 1; } ;;
esac
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var progress strings.Builder
	env := hermeticEnv(os.Environ(), filepath.Join(dir, "config"), filepath.Join(dir, "state"))
	_, _, failed, err := measureSize(litBinary{path: path}, dir, size{name: "one", rows: 1}, env, limits{budget: time.Second}, runQuiet, &progress)
	if err != nil {
		t.Fatalf("measureSize error = %v; a failed writer is a verdict, not a broken run", err)
	}
	if len(failed) != 1 || failed[0].writer != 7 {
		t.Fatalf("failed writers = %v, want writer 7 alone", failed)
	}
	logged := progress.String()
	if !strings.Contains(logged, "WRITER FAILED: store one (1 rows): writer 7 of 100 did not complete") || !strings.Contains(logged, "workspace busy") {
		t.Errorf("writer 7's failure and its stderr were not logged:\n%s", logged)
	}
	if strings.Count(logged, "WRITER FAILED") != 1 {
		t.Errorf("progress logged writers that completed:\n%s", logged)
	}
}

// The report's CONTENTION section lists every failed writer, and says so in as
// many words when there are none: a section that prints nothing reads the same
// as a section that never ran.
func TestRenderContentionStatesTheVerdict(t *testing.T) {
	failure := writerFailure{store: size{name: "n=500", rows: 500}, writer: 12, err: errors.New("exit status 1")}
	got := renderContention([]writerFailure{failure})
	if !strings.Contains(got, "FAILED  store n=500 (500 rows): writer 12 of 100 did not complete: exit status 1") {
		t.Errorf("report does not name the failed writer:\n%s", got)
	}
	if got := renderContention(nil); !strings.Contains(got, "every writer completed") {
		t.Errorf("report over a clean storm does not say so:\n%s", got)
	}
}
