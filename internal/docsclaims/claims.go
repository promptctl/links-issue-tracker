// Package docsclaims ties the repo's user-facing storage prose to the
// event-store migration state that will falsify it.
//
// README.md and docs/architecture.md say, truthfully today, that issues live
// in an embedded Dolt database. design-docs/event-store/design.md §migration
// plans the states (S0..S4) that end with that machinery deleted, and its
// per-state table is where a state's advance is recorded. Nothing else
// connected the two: a released README could describe storage the binary no
// longer has, and only a manual sweep would notice.
//
// [LAW:one-source-of-truth] Campaign state is read from the §migration table
// itself — this package holds no copy of which states are built. What it does
// hold is the registry of user-facing claims, each naming the state whose
// completion makes it false. The gate test checks both directions: while the
// state is unbuilt the claim must still be present (so rewording a doc forces
// a registry update instead of silently retiring the guard), and once the
// state is built the claim must be gone.
package docsclaims

import (
	"fmt"
	"regexp"
	"strings"
)

// DesignDoc is the repo-relative path of the design doc whose §migration
// table records campaign state.
const DesignDoc = "design-docs/event-store/design.md"

// A Claim is one user-facing statement that stops being true when a planned
// migration state is built.
type Claim struct {
	File        string // repo-relative path of the doc making the statement
	Quote       string // verbatim fragment of the statement; matched whitespace-insensitively
	FalsifiedBy string // migration state (e.g. "S3") whose completion falsifies it
}

// Claims registers every user-facing statement that describes Dolt as lit's
// storage rather than as one engine behind the storage contract. All are
// falsified by S3 (events become truth, sync becomes git refs, Dolt shadows
// as rollback) — S4 only makes them more false.
var Claims = []Claim{
	{
		File:        "README.md",
		Quote:       "Issues live in an embedded [Dolt](https://www.dolthub.com/) database",
		FalsifiedBy: "S3",
	},
	{
		File:        "README.md",
		Quote:       "State is a Dolt database, so issue history is real",
		FalsifiedBy: "S3",
	},
	{
		File:        "README.md",
		Quote:       "issues are rows in an embedded Dolt SQL database",
		FalsifiedBy: "S3",
	},
	{
		File:        "README.md",
		Quote:       "`lit sync` mirrors that Dolt data through your existing git remotes",
		FalsifiedBy: "S3",
	},
	{
		File:        "docs/architecture.md",
		Quote:       "`links` uses Dolt as an embedded SQL database with commit semantics",
		FalsifiedBy: "S3",
	},
	{
		File:        "docs/architecture.md",
		Quote:       "working set is committed",
		FalsifiedBy: "S3",
	},
	{
		File:        "docs/architecture.md",
		Quote:       "Sync uses Dolt remotes",
		FalsifiedBy: "S3",
	},
	{
		File:        "docs/architecture.md",
		Quote:       "reconciles Dolt remotes from `git remote -v` fetch URLs",
		FalsifiedBy: "S3",
	},
}

// stateCell matches the first cell of a per-state table row: a state id like
// "S3" followed by its name ("S3 write-flip"). Prose mentioning a state never
// sits in a table row's first cell, so the anchor is the row shape, not the
// token.
var stateCell = regexp.MustCompile(`^S\d+\b`)

// StateStatuses parses every per-state table row in doc into a map from state
// id ("S0") to its Status cell ("built (v0.9.0)", "destination"). design.md
// says §migration holds the only such table; if a second one ever appears, the
// duplicate-state error below surfaces it instead of last-row-wins hiding it.
func StateStatuses(doc string) (map[string]string, error) {
	statuses := map[string]string{}
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := splitRow(line)
		if len(cells) == 0 || !stateCell.MatchString(cells[0]) {
			continue
		}
		// [LAW:no-silent-failure] A state row that doesn't have the table's
		// four columns is a restructured table, not a row to skip.
		if len(cells) != 4 {
			return nil, fmt.Errorf("state row %q has %d cells, want 4 (State | The system is | Gate to advance | Status); if the §migration table changed shape, update internal/docsclaims", line, len(cells))
		}
		id := strings.Fields(cells[0])[0]
		if _, dup := statuses[id]; dup {
			return nil, fmt.Errorf("state %s appears in more than one table row; internal/docsclaims assumes §migration holds the only per-state table", id)
		}
		statuses[id] = cells[3]
	}
	return statuses, nil
}

// splitRow returns the trimmed cells of a markdown table row.
func splitRow(line string) []string {
	parts := strings.Split(strings.Trim(line, "|"), "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// Normalize collapses whitespace runs so a re-wrapped paragraph still carries
// the same claim. [LAW:behavior-not-structure] The claim is the contract; the
// line wrapping is structure the gate must not pin.
func Normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Violation reports what is wrong with the claim given its doc's content and
// its falsifying state's status, or "" when doc and state agree. Any status
// other than the literal "destination" counts as built — "built (v0.9.0)",
// "built (unreleased)", and a status this code has never seen all mean a
// human must look at the docs, so an unrecognized value trips the gate rather
// than passing it. [LAW:no-silent-failure]
func (c Claim) Violation(status, docContent string) string {
	present := strings.Contains(Normalize(docContent), Normalize(c.Quote))
	unbuilt := status == "destination"
	switch {
	case unbuilt && !present:
		return fmt.Sprintf("%s no longer contains %q while state %s is still unbuilt; if the claim was reworded or removed, update the registry in internal/docsclaims", c.File, c.Quote, c.FalsifiedBy)
	case !unbuilt && present:
		return fmt.Sprintf("%s still claims %q, but migration state %s is now %q — the claim is false; rewrite that prose for the post-%s world and drop it from the registry in internal/docsclaims", c.File, c.Quote, c.FalsifiedBy, status, c.FalsifiedBy)
	}
	return ""
}
