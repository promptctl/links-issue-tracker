package docsclaims

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUserFacingDoltClaimsTrackMigrationState is the gate
// ([LAW:verifiable-goals], [LAW:single-enforcer]): it reads the §migration
// table and every registered doc, and fails naming each claim that disagrees
// with its falsifying state — present after the state is built, or missing
// while it isn't. Flipping a state's status in design.md (which design.md
// says is part of closing the work that ships it) makes this test the loud
// reminder that README.md and docs/architecture.md now lie.
func TestUserFacingDoltClaimsTrackMigrationState(t *testing.T) {
	statuses, err := StateStatuses(readRepoFile(t, DesignDoc))
	if err != nil {
		t.Fatalf("parsing %s: %v", DesignDoc, err)
	}
	for _, c := range Claims {
		status, ok := statuses[c.FalsifiedBy]
		if !ok {
			t.Errorf("claim %q in %s names migration state %s, which the §migration table in %s does not carry", c.Quote, c.File, c.FalsifiedBy, DesignDoc)
			continue
		}
		if v := c.Violation(status, readRepoFile(t, c.File)); v != "" {
			t.Error(v)
		}
	}
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(data)
}

// The fixture mirrors the real table's shape: header, separator, one built
// state, one unbuilt, with bold markup and prose mentioning a state id
// outside any table row.
const tableFixture = `## §migration — five states, four gates
status: destination

| State | The system is | Gate to advance | Status |
|---|---|---|---|
| S0 seam | CLI depends on an interface | interface carved | built (v0.9.0) |
| S3 write-flip | events are **truth** | rollback window quiet | destination |

S0 shipped as four things, and S3 follows it.
`

func TestStateStatusesReadsRowsNotProse(t *testing.T) {
	statuses, err := StateStatuses(tableFixture)
	if err != nil {
		t.Fatalf("StateStatuses: %v", err)
	}
	want := map[string]string{"S0": "built (v0.9.0)", "S3": "destination"}
	if len(statuses) != len(want) {
		t.Fatalf("parsed %d states %v, want %d — header, separator, and prose mentions must not parse as states", len(statuses), statuses, len(want))
	}
	for id, status := range want {
		if statuses[id] != status {
			t.Errorf("state %s = %q, want %q", id, statuses[id], status)
		}
	}
}

func TestStateStatusesToleratesEdgeWhitespace(t *testing.T) {
	doc := "  | S2 read-flip | reads from fold | flag default-on | destination |  \n"
	statuses, err := StateStatuses(doc)
	if err != nil {
		t.Fatalf("StateStatuses: %v — edge whitespace must not misreport the table as reshaped", err)
	}
	if statuses["S2"] != "destination" {
		t.Errorf("state S2 = %q, want %q — an indented row must parse, not be skipped", statuses["S2"], "destination")
	}
}

func TestStateStatusesRejectsMalformedTables(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"duplicate state across rows", tableFixture + "\n| S3 shadow | again | again | destination |\n"},
		{"state row with too few cells", "| S1 shadow | destination |\n"},
		{"state row with too many cells", "| S1 shadow | a | b | c | destination |\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := StateStatuses(tc.doc); err == nil {
				t.Errorf("StateStatuses accepted %q; a reshaped table must fail loudly, not parse approximately", tc.doc)
			}
		})
	}
}

func TestViolationCoversBothDirections(t *testing.T) {
	c := Claim{File: "README.md", Quote: "issues live in Dolt", FalsifiedBy: "S3"}
	cases := []struct {
		name    string
		status  string
		doc     string
		violate bool
	}{
		{"unbuilt state, claim present", "destination", "today, issues live in Dolt, embedded", false},
		{"unbuilt state, claim rewrapped across lines", "destination", "today, issues live\nin  Dolt, embedded", false},
		{"unbuilt state, claim reworded", "destination", "issues live in a local database", true},
		{"built state, claim still present", "built (unreleased)", "today, issues live in Dolt, embedded", true},
		{"built state, claim gone", "built (v1.2.0)", "issues live in a local database", false},
		{"unrecognized status counts as built", "destinaton", "today, issues live in Dolt, embedded", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := c.Violation(tc.status, tc.doc)
			if got := v != ""; got != tc.violate {
				t.Errorf("Violation(%q, %q) = %q, want violation=%t", tc.status, tc.doc, v, tc.violate)
			}
			if tc.violate && !strings.Contains(v, c.File) {
				t.Errorf("violation %q does not name the file to fix", v)
			}
		})
	}
}
