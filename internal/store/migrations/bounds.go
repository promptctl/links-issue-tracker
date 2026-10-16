package migrations

import (
	"cmp"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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
// does not begin with `<digits>_` naming a version of at least 1.
//
// [LAW:types-are-the-program] The accept-shape is exact: a non-empty prefix
// of ASCII digits followed by '_', naming a number goose accepts as a version
// (its NumericComponent refuses anything below 1). fmt.Sscanf("%d") would have accepted
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
	if err != nil || version < 1 {
		return 0, false
	}
	return version, true
}

// Migration is one registry file: the goose version its name begins with, and
// the name.
type Migration struct {
	Version int64
	Name    string
}

// Registry is a migration registry: every *.sql file in its directory, each
// with the version its name begins with, in strictly ascending version order.
// It holds at least one migration.
//
// [LAW:parse-dont-validate] ParseRegistry is the only constructor, so a
// Registry is proof the registry rule held; nothing that holds one re-applies it.
// The zero Registry holds no migrations and is not a registry.
type Registry struct {
	migrations []Migration
}

// ParseRegistry applies the registry rule to the file names in a migration
// directory: the *.sql files are the migrations, every one begins with a
// version, no two share a version, and there is at least one. Other files
// (README.md, Go sources) are not migrations.
//
// [LAW:one-source-of-truth] The one statement of the rule. The embedded
// registry and a release tag's registry (tools/schema-release-check) are both
// read through it, and it refuses every registry goose's provider refuses
// (version below 1, a version claimed twice), so no registry it accepts fails
// to open.
func ParseRegistry(names []string) (Registry, error) {
	var ms []Migration
	for _, name := range names {
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		v, ok := ParseVersion(name)
		if !ok {
			return Registry{}, fmt.Errorf("migration file %q does not begin with a version (a number of at least 1, then '_')", name)
		}
		ms = append(ms, Migration{Version: v, Name: name})
	}
	if len(ms) == 0 {
		return Registry{}, errors.New("migration registry is empty")
	}
	slices.SortFunc(ms, func(a, b Migration) int {
		return cmp.Or(cmp.Compare(a.Version, b.Version), strings.Compare(a.Name, b.Name))
	})
	for i := 1; i < len(ms); i++ {
		if ms[i].Version == ms[i-1].Version {
			return Registry{}, fmt.Errorf("migration version %d is claimed by both %q and %q", ms[i].Version, ms[i-1].Name, ms[i].Name)
		}
	}
	return Registry{migrations: ms}, nil
}

// Embedded is the registry FS embeds: the one the runner applies.
func Embedded() (Registry, error) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		return Registry{}, fmt.Errorf("read migration registry: %w", err)
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return ParseRegistry(names)
}

// Migrations is every migration in the registry, in ascending version order.
func (r Registry) Migrations() []Migration {
	return slices.Clone(r.migrations)
}

// MaxVersion is the highest version in the registry.
func (r Registry) MaxVersion() int64 {
	return r.migrations[len(r.migrations)-1].Version
}

// MaxVersion returns the highest version in the embedded registry. It bounds
// "pending" without touching the database and bounds "schema support" for
// internal/version without hand-maintaining a constant.
//
// [LAW:one-source-of-truth] Adding a migration changes this return value
// automatically; no parallel constant elsewhere to forget.
func MaxVersion() (int64, error) {
	r, err := Embedded()
	if err != nil {
		return 0, err
	}
	return r.MaxVersion(), nil
}

// BaselineFileName is the registry file whose version is Baseline.
func BaselineFileName() (string, error) {
	r, err := Embedded()
	if err != nil {
		return "", err
	}
	for _, m := range r.migrations {
		if m.Version == Baseline {
			return m.Name, nil
		}
	}
	return "", fmt.Errorf("no baseline migration (v%d) found in registry", Baseline)
}
