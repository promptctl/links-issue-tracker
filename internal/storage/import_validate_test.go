package storage

import (
	"errors"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// A file's references are judged by the rule that names the fix: a
// blocked_by along the hierarchy the file describes is refused with the
// same-epic message in either direction, though the epic-blocked-by-its-own-
// child direction is also a cycle in write order, and so is one between two
// leaves of one epic in the file. Each refusal sits beside a shape the gate
// accepts, so a gate that refused everything would fail here.
func TestImportGateJudgesTheFilesReferences(t *testing.T) {
	str := func(s string) *string { return &s }
	tree := func(localID, issueType, parent string, blockedBy ...string) ImportTreeSpec {
		return ImportTreeSpec{LocalID: localID, Title: localID, IssueType: issueType, Topic: "core", Parent: parent, BlockedBy: blockedBy}
	}
	create := func(localID, issueType, parent string, blockedBy ...string) BulkIssueSpec {
		return BulkIssueSpec{LocalID: localID, Title: str("title"), IssueType: str(issueType), Topic: str("core"), Parent: parent, BlockedBy: blockedBy}
	}
	sameEpic := ": " + SameEpicBlocksRejectionMessage
	cases := []struct {
		name string
		gate func() error
		want string // "" when the gate accepts the file
	}{
		{"tree: a task blocked by the epic it sits under", func() error {
			_, err := ImportTreeWriteOrder([]ImportTreeSpec{tree("e", "epic", ""), tree("t", "task", "e", "e")})
			return err
		}, `import: spec "t" blocked_by "e"` + sameEpic},
		{"tree: an epic blocked by a task under it", func() error {
			_, err := ImportTreeWriteOrder([]ImportTreeSpec{tree("e", "epic", "", "t"), tree("t", "task", "e")})
			return err
		}, `import: spec "e" blocked_by "t"` + sameEpic},
		{"tree: a task blocked by an epic two levels up", func() error {
			_, err := ImportTreeWriteOrder([]ImportTreeSpec{tree("e", "epic", ""), tree("s", "epic", "e"), tree("t", "task", "s", "e")})
			return err
		}, `import: spec "t" blocked_by "e"` + sameEpic},
		{"tree: two leaves of one epic, one blocked by the other", func() error {
			_, err := ImportTreeWriteOrder([]ImportTreeSpec{tree("e", "epic", ""), tree("a", "task", "e"), tree("b", "bug", "e", "a")})
			return err
		}, `import: spec "b" blocked_by "a"` + sameEpic},
		{"tree: leaves of two epics, one blocked by the other, is accepted", func() error {
			_, err := ImportTreeWriteOrder([]ImportTreeSpec{tree("e", "epic", ""), tree("f", "epic", ""), tree("a", "task", "e"), tree("b", "task", "f", "a")})
			return err
		}, ""},
		{"tree: a leaf and the sub-epic beside it, each way round, is accepted", func() error {
			_, err := ImportTreeWriteOrder([]ImportTreeSpec{tree("e", "epic", ""), tree("s", "epic", "e", "a"), tree("a", "task", "e"), tree("b", "task", "e", "s")})
			return err
		}, ""},
		{"tree: two epics blocking each other is a cycle", func() error {
			_, err := ImportTreeWriteOrder([]ImportTreeSpec{tree("a", "epic", "", "b"), tree("b", "epic", "", "a")})
			return err
		}, `import: cycle detected involving "a"`},
		{"tree: a blocked_by naming one record twice", func() error {
			_, err := ImportTreeWriteOrder([]ImportTreeSpec{tree("a", "epic", ""), tree("b", "epic", "", "a", "a")})
			return err
		}, `import: spec "b": blocked_by names "a" twice`},
		{"tree: a blocked_by naming two records once each is accepted", func() error {
			_, err := ImportTreeWriteOrder([]ImportTreeSpec{tree("a", "epic", ""), tree("c", "epic", ""), tree("b", "epic", "", "a", "c")})
			return err
		}, ""},
		{"bulk: a task with no local_id blocked by the epic it sits under", func() error {
			_, err := BulkWriteOrder([]BulkIssueSpec{create("e", "epic", ""), create("", "task", "e", "e")})
			return err
		}, `bulk: doc 1 blocked_by "e"` + sameEpic},
		{"bulk: an epic blocked by a task under it", func() error {
			_, err := BulkWriteOrder([]BulkIssueSpec{create("e", "epic", "", "t"), create("t", "task", "e")})
			return err
		}, `bulk: doc 0 blocked_by "t"` + sameEpic},
		{"bulk: a task with no local_id blocked by a task beside it", func() error {
			_, err := BulkWriteOrder([]BulkIssueSpec{create("e", "epic", ""), create("a", "task", "e"), create("", "chore", "e", "a")})
			return err
		}, `bulk: doc 2 blocked_by "a"` + sameEpic},
		{"bulk: a task blocked by a real id, which the write judges, is accepted", func() error {
			_, err := BulkWriteOrder([]BulkIssueSpec{create("e", "epic", ""), create("t", "task", "e", "lit-real-1")})
			return err
		}, ""},
		{"bulk: a create naming one real id twice in blocked_by", func() error {
			_, err := BulkWriteOrder([]BulkIssueSpec{create("e", "epic", "", "lit-real-1", "lit-real-1")})
			return err
		}, `bulk: doc 0: blocked_by names "lit-real-1" twice`},
		{"bulk: an update with a doubled blocked_by is told update takes none", func() error {
			_, err := BulkWriteOrder([]BulkIssueSpec{{ID: "lit-real-1", Title: str("t"), BlockedBy: []string{"lit-real-2", "lit-real-2"}}})
			return err
		}, `bulk: doc 0 (id "lit-real-1") sets blocked_by; wire dependencies with ` + "`lit dep add`" + ` instead`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.gate()
			if c.want == "" {
				if err != nil {
					t.Fatalf("gate error = %v, want the file accepted", err)
				}
				return
			}
			var refused model.ValidationError
			if !errors.As(err, &refused) {
				t.Fatalf("gate error = %v, want a model.ValidationError", err)
			}
			if err.Error() != c.want {
				t.Errorf("gate error =\n  %s\nwant\n  %s", err, c.want)
			}
		})
	}
}
