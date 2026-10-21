package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// tempTableFilePrefix is the name Dolt gives each table file it stages in
// TempTableFilesDir: nbs.NewBufferedFileByteSink creates it through
// os.CreateTemp with this pattern (store/nbs/byte_sink.go:222 in the pinned
// promptctl/dolt fork), so the name is this prefix and a decimal number. Dolt
// exports neither the prefix nor a way to ask for it.
const tempTableFilePrefix = "buffered_file_byte_sink_"

// collectDeadTempTableFiles removes every table file a dead writer left
// staged in the store's TempTableFilesDir. Dolt removes a staged file only
// once it has uploaded it (store/datas/pull/pull_table_file_writer.go:318-335):
// a push, fetch or landing that fails, is cut short, or is killed leaves its
// file behind, and nothing else ever removes it — Dolt's own age-based sweep
// at env load builds its path from the database URL and never finds the
// directory (env/environment.go:283).
//
// The caller must hold Dolt's journal LOCK through its own write engine and
// have started no push, fetch or landing on it. That hold is the proof every
// staged file is dead: each of those writers stages through an engine, and an
// engine holds LOCK from its open to its close, so while this engine holds
// it, no other engine can be staging, and this one is not. A Store that
// rotates its engine (doc.go's reconnect and withEngineClosed) is no
// exception: what its closed engine staged died with it.
// [LAW:no-ambient-temporal-coupling] the kernel owns the liveness answer — no
// age, no PID.
//
// Only a regular file with exactly Dolt's staged-file name is collected;
// everything else in the directory (dolt_embedded_metrics, anything foreign)
// stays. A directory Dolt has not made yet holds nothing to collect. Neither
// a file that cannot be removed nor a read that fails part-way stops the
// rest: the collection removes every staged file it read, reports why the
// rest stayed, and the next writer's open tries again.
func collectDeadTempTableFiles(doltRootDir string) storage.TempTableFilesCollection {
	dir := TempTableFilesDir(doltRootDir)
	// [LAW:dataflow-not-control-flow] ReadDir returns what it read beside its
	// error, so the loop always runs over those entries and a failed read is
	// one more failure.
	entries, err := os.ReadDir(dir)
	var failures []error
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		failures = append(failures, fmt.Errorf("read the temp table file dir: %w", err))
	}
	var collected storage.TempTableFilesCollection
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !isStagedTableFileName(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			failures = append(failures, fmt.Errorf("stat dead temp table file %s: %w", path, err))
			continue
		}
		if err := os.Remove(path); err != nil {
			failures = append(failures, fmt.Errorf("remove dead temp table file %s: %w", path, err))
			continue
		}
		collected.Files++
		collected.Bytes += info.Size()
	}
	collected.Err = errors.Join(failures...)
	return collected
}

// isStagedTableFileName reports whether name is one Dolt gives a table file it
// stages: tempTableFilePrefix and then one or more ASCII digits, nothing else.
func isStagedTableFileName(name string) bool {
	digits, ok := strings.CutPrefix(name, tempTableFilePrefix)
	return ok && digits != "" && strings.Trim(digits, "0123456789") == ""
}
