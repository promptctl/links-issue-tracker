package lawtokens

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/lawtokens/tokenindex"
)

// Violation is a non-canonical marker and the file it was found in.
type Violation struct {
	Path   string
	Marker Marker
}

// String renders the violation as "path:line: [NAMESPACE:token]".
func (v Violation) String() string {
	return v.Path + ":" + strconv.Itoa(v.Marker.Line) + ": " + v.Marker.String()
}

// CheckFiles scans each named file in fsys and returns every non-canonical
// marker, in the order given. It is the one check behind both enforcement
// points: the pre-commit hook passes it the staged files, and
// TestRepoMarkersAreCanonical passes it every tracked file.
// [LAW:single-enforcer]
//
// A file that cannot be read is an error, never a file with nothing in it: a
// check that skipped it would pass without having looked. [LAW:no-silent-failure]
func CheckFiles(fsys fs.FS, paths []string) ([]Violation, error) {
	var violations []Violation
	for _, path := range paths {
		content, err := fs.ReadFile(fsys, path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		for _, m := range NonCanonical(ScanMarkers(string(content))) {
			violations = append(violations, Violation{Path: path, Marker: m})
		}
	}
	return violations, nil
}

// Report is the failure text for a non-empty set of violations: each one on
// its own line, then what to do about it.
func Report(violations []Violation) string {
	lines := make([]string, len(violations))
	for i, v := range violations {
		lines[i] = "  " + v.String()
	}
	return fmt.Sprintf("found %d [LAW]/[FRAMING] marker(s) whose token is not in lawtokens.Canonical:\n%s\n\n"+
		"This repository allows only canonical law tokens; never invent one. If a "+
		"situation seems to need a new token, it is an instance of an existing law: "+
		"cite that law. Canonical is generated from the upstream Token index (%s). "+
		"If that index lists the token, the generated copy is behind: run `just "+
		"lawtokens-sync` and commit the result, and do not replace a correct citation "+
		"with an older token. If it does not, fix the token.\n",
		len(violations), strings.Join(lines, "\n"), tokenindex.UpstreamURL)
}
