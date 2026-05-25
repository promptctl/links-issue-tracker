package migrations

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Baseline is the schema version 00001_baseline.sql stamps. A pre-goose
// workspace already at the baseline shape is adopted by recording this version
// without re-running the CREATE TABLEs.
//
// [LAW:one-source-of-truth] The baseline is a property of the embedded registry
// (it is the lowest version in FS), not a parallel constant maintained in
// consumer packages. Code that needs "what is the baseline" reads it here.
const Baseline int64 = 1

// ParseVersion extracts the leading numeric version from a goose migration
// filename (e.g. "00002_add_foo.sql" -> 2). Returns false for any name that
// does not begin with `<digits>_`.
//
// [LAW:types-are-the-program] The accept-shape is exact: a non-empty prefix
// of ASCII digits followed by '_'. fmt.Sscanf("%d") would have accepted
// leading whitespace, signs, or strings like "00001a" (which it would parse
// as 1, leaving "a" unread); strconv.ParseInt on a digit-validated substring
// rejects every shape the producer (goose, by convention) does not emit.
func ParseVersion(name string) (int64, bool) {
	base := filepath.Base(name)
	idx := strings.IndexByte(base, '_')
	if idx <= 0 {
		return 0, false
	}
	digits := base[:idx]
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	version, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return version, true
}

// MaxVersion returns the highest version in the embedded registry. It bounds
// "pending" without touching the database and bounds "schema support" for
// internal/version without hand-maintaining a constant.
//
// [LAW:one-source-of-truth] Adding a migration changes this return value
// automatically; no parallel constant elsewhere to forget.
func MaxVersion() (int64, error) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		return 0, fmt.Errorf("read migration registry: %w", err)
	}
	var versions []int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		v, ok := ParseVersion(entry.Name())
		if !ok {
			return 0, fmt.Errorf("migration file %q does not begin with a numeric version", entry.Name())
		}
		versions = append(versions, v)
	}
	if len(versions) == 0 {
		return 0, errors.New("migration registry is empty")
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	return versions[len(versions)-1], nil
}

// BaselineFileName is the registry file whose version is Baseline.
func BaselineFileName() (string, error) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		return "", fmt.Errorf("read migration registry: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if v, ok := ParseVersion(entry.Name()); ok && v == Baseline {
			return entry.Name(), nil
		}
	}
	return "", fmt.Errorf("no baseline migration (v%d) found in registry", Baseline)
}
