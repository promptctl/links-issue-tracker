// Package docnames checks that every Go name the v1 specification writes in a
// code span is an identifier somewhere in this repository's source.
//
// doc-v1-total is what an agent reads to find code it has not seen, so a
// confident `runSyncPush` is followed with confidence. A name that exists
// nowhere sends that reader searching for a successor, and the reasonable guess
// that it was renamed is wrong when it never existed. No line citation guards
// against this, because a name that cites nothing has nothing to resolve.
// [LAW:one-source-of-truth] the source tree is the fact; a named identifier in
// a chapter is a map of it, and this package is what re-checks the map.
//
// What counts as a Go name is decided by shape alone: a code span of one or
// more dot-joined identifiers, optionally followed by a call's parenthesized
// arguments, in which some identifier mixes upper and lower case. Lowercase
// words (`open`, `dolt_log()`) and all-caps words (`GOCACHE`, `VARCHAR(64)`)
// are left alone. In this corpus they are column names, statuses, commands,
// SQL and environment variables, which the shape cannot tell apart from Go.
// A call's arguments are not judged.
//
// What counts as existing is an identifier token in any Go file of the source
// this repository carries, tests and tools included. A name that survives only
// in a comment or a string does not count, because a comment naming a deleted
// function is exactly the residue the check is for.
//
// Some names are legitimately not in the tree: a standard-library or
// third-party symbol, an agent-harness hook, a unit or a format pattern. Those
// are listed by name, one per line with the reason, in ExclusionsFile. The list is data
// rather than conditionals so the next chapter author extends it by adding a
// line, and the gate keeps it honest in both directions: an entry no chapter
// still writes, or one the tree now declares, is reported as stale.
package docnames

import (
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/promptctl/links-issue-tracker/internal/docclaims"
)

// ExclusionsFile lists the names a chapter may write that are not identifiers
// in this tree. It lives beside the chapters because a chapter's author is the
// one who adds to it.
const ExclusionsFile = docclaims.SpecDir + "/names-outside-the-tree.txt"

// goName is a span shaped like a Go name: dot-joined identifiers, optionally
// followed by a call's parenthesized arguments, which are not judged.
var goName = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)(\(.*\))?$`)

// Name is one Go-shaped span in a chapter.
type Name struct {
	Doc  string
	Line int
	// Text is the span as written; Name is its dotted name without any call.
	Text string
	Name string
}

// parseName reports whether a span names Go, and the dotted name it writes.
func parseName(doc string, s docclaims.Span) (Name, bool) {
	m := goName.FindStringSubmatch(s.Text)
	if m == nil {
		return Name{}, false
	}
	if !slices.ContainsFunc(strings.Split(m[1], "."), mixedCase) {
		return Name{}, false
	}
	return Name{Doc: doc, Line: s.Line, Text: s.Text, Name: m[1]}, true
}

func mixedCase(s string) bool {
	return strings.ToLower(s) != s && strings.ToUpper(s) != s
}

// Names reads the Go-shaped spans of the given chapters.
func Names(fsys fs.FS, docs []string) ([]Name, error) {
	var out []Name
	for _, doc := range docs {
		src, err := fs.ReadFile(fsys, doc)
		if err != nil {
			return nil, err
		}
		spans, err := docclaims.CodeSpans(string(src))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", doc, err)
		}
		for _, s := range spans {
			if n, ok := parseName(doc, s); ok {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// Identifiers collects every identifier and keyword token in the Go source this repository
// carries. A directory holding its own go.mod is another module and is walked
// only if go.mod replaces a dependency onto it; directories the go tool ignores
// by name (leading "." or "_", testdata) are skipped as the go tool skips them.
func Identifiers(fsys fs.FS) (map[string]bool, error) {
	roots, err := docclaims.SourceDirs(fsys)
	if err != nil {
		return nil, err
	}
	idents := map[string]bool{}
	for _, root := range roots {
		err := fs.WalkDir(fsys, root, func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name != root && (ignoredByGo(d.Name()) || isModuleRoot(fsys, name)) {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || ignoredByGo(d.Name()) {
				return nil
			}
			return scanIdents(fsys, name, idents)
		})
		if err != nil {
			return nil, err
		}
	}
	// An empty set would report every name in the corpus as missing — the
	// whole specification false rather than this walk broken.
	// [LAW:no-silent-failure]
	if len(idents) == 0 {
		return nil, fmt.Errorf("no Go identifiers found under %v", roots)
	}
	return idents, nil
}

func ignoredByGo(base string) bool {
	return strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || base == "testdata"
}

func isModuleRoot(fsys fs.FS, dir string) bool {
	_, err := fs.Stat(fsys, path.Join(dir, "go.mod"))
	return err == nil
}

// scanIdents adds one file's identifier tokens. Tokenizing rather than parsing
// means a file excluded from every build still contributes: its names are in
// the tree for a reader to find, which is the question asked.
func scanIdents(fsys fs.FS, name string, into map[string]bool) error {
	src, err := fs.ReadFile(fsys, name)
	if err != nil {
		return err
	}
	var errs scanner.ErrorList
	fset := token.NewFileSet()
	var s scanner.Scanner
	s.Init(fset.AddFile(name, -1, len(src)), src, errs.Add, 0)
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.IDENT || tok.IsKeyword() {
			into[lit] = true
		}
	}
	// A file that does not tokenize would silently drop its names, and each
	// would report as a chapter naming something that does not exist.
	// [LAW:no-silent-failure]
	return errs.Err()
}

// Exclusion is one line of ExclusionsFile: a dotted name as chapters write it,
// without any call, and why it is not in the tree.
type Exclusion struct {
	Line int
	Name string
	Why  string
}

// ParseExclusions reads ExclusionsFile. Blank lines and lines starting with
// "#" are commentary; every other line is a name, whitespace, then the reason.
// A line with no reason is refused, because the reason is what lets the next
// reader judge whether the entry still holds.
func ParseExclusions(src string) ([]Exclusion, error) {
	var out []Exclusion
	seen := map[string]int{}
	for i, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		text, why := line, ""
		if i := strings.IndexFunc(line, unicode.IsSpace); i >= 0 {
			text, why = line[:i], strings.TrimSpace(line[i:])
		}
		// An entry that is not a bare name could never match one, so it would
		// sit in the list excusing nothing. [LAW:parse-dont-validate]
		if m := goName.FindStringSubmatch(text); m == nil || m[2] != "" {
			return nil, fmt.Errorf("%s:%d: `%s` is not a dotted name; list the name a chapter writes, without its call", ExclusionsFile, i+1, text)
		}
		if why == "" {
			return nil, fmt.Errorf("%s:%d: `%s` gives no reason it is outside the tree", ExclusionsFile, i+1, text)
		}
		if prev, dup := seen[text]; dup {
			return nil, fmt.Errorf("%s:%d: `%s` is already listed at line %d", ExclusionsFile, i+1, text, prev)
		}
		seen[text] = i + 1
		out = append(out, Exclusion{Line: i + 1, Name: text, Why: why})
	}
	return out, nil
}

// StaleExclusion is an entry that no longer excuses anything.
type StaleExclusion struct {
	Exclusion
	// Declared is true when the tree now has every identifier the entry names,
	// false when no chapter writes the name any more.
	Declared bool
}

// Report is the outcome of one check.
type Report struct {
	// Missing are the chapter names with a segment no Go file declares or
	// uses, in chapter order.
	Missing []Name
	Stale   []StaleExclusion
}

// Clean reports whether the check found nothing.
func (r Report) Clean() bool { return len(r.Missing) == 0 && len(r.Stale) == 0 }

// Check compares the chapters' names against the tree's identifiers. It is a
// pure function of its inputs so a test can hand it a fabricated corpus.
func Check(names []Name, idents map[string]bool, exclusions []Exclusion) Report {
	excluded := map[string]bool{}
	for _, e := range exclusions {
		excluded[e.Name] = true
	}
	written := map[string]bool{}
	var r Report
	for _, n := range names {
		written[n.Name] = true
		if !excluded[n.Name] && !declared(n.Name, idents) {
			r.Missing = append(r.Missing, n)
		}
	}
	for _, e := range exclusions {
		switch {
		case !written[e.Name]:
			r.Stale = append(r.Stale, StaleExclusion{Exclusion: e})
		case declared(e.Name, idents):
			r.Stale = append(r.Stale, StaleExclusion{Exclusion: e, Declared: true})
		}
	}
	return r
}

// declared reports whether every identifier of a dotted name is in the tree.
func declared(name string, idents map[string]bool) bool {
	return !slices.ContainsFunc(strings.Split(name, "."), func(s string) bool { return !idents[s] })
}

// CheckTree runs the check over the specification in fsys.
func CheckTree(fsys fs.FS) (Report, error) {
	docs, err := docclaims.SpecFiles(fsys)
	if err != nil {
		return Report{}, err
	}
	names, err := Names(fsys, docs)
	if err != nil {
		return Report{}, err
	}
	idents, err := Identifiers(fsys)
	if err != nil {
		return Report{}, err
	}
	src, err := fs.ReadFile(fsys, ExclusionsFile)
	if err != nil {
		return Report{}, err
	}
	exclusions, err := ParseExclusions(string(src))
	if err != nil {
		return Report{}, err
	}
	return Check(names, idents, exclusions), nil
}
