package storage

import (
	"errors"
	"strings"
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
			_, err := ParseImportTreeFile([]ImportTreeSpec{tree("e", "epic", ""), tree("t", "task", "e", "e")})
			return err
		}, `import: spec "t" blocked_by "e"` + sameEpic},
		{"tree: an epic blocked by a task under it", func() error {
			_, err := ParseImportTreeFile([]ImportTreeSpec{tree("e", "epic", "", "t"), tree("t", "task", "e")})
			return err
		}, `import: spec "e" blocked_by "t"` + sameEpic},
		{"tree: a task blocked by an epic two levels up", func() error {
			_, err := ParseImportTreeFile([]ImportTreeSpec{tree("e", "epic", ""), tree("s", "epic", "e"), tree("t", "task", "s", "e")})
			return err
		}, `import: spec "t" blocked_by "e"` + sameEpic},
		{"tree: two leaves of one epic, one blocked by the other", func() error {
			_, err := ParseImportTreeFile([]ImportTreeSpec{tree("e", "epic", ""), tree("a", "task", "e"), tree("b", "bug", "e", "a")})
			return err
		}, `import: spec "b" blocked_by "a"` + sameEpic},
		{"tree: leaves of two epics, one blocked by the other, is accepted", func() error {
			_, err := ParseImportTreeFile([]ImportTreeSpec{tree("e", "epic", ""), tree("f", "epic", ""), tree("a", "task", "e"), tree("b", "task", "f", "a")})
			return err
		}, ""},
		{"tree: a leaf and the sub-epic beside it, each way round, is accepted", func() error {
			_, err := ParseImportTreeFile([]ImportTreeSpec{tree("e", "epic", ""), tree("s", "epic", "e", "a"), tree("a", "task", "e"), tree("b", "task", "e", "s")})
			return err
		}, ""},
		{"tree: two epics blocking each other is a cycle", func() error {
			_, err := ParseImportTreeFile([]ImportTreeSpec{tree("a", "epic", "", "b"), tree("b", "epic", "", "a")})
			return err
		}, `import: cycle detected involving "a"`},
		{"tree: a blocked_by naming one record twice", func() error {
			_, err := ParseImportTreeFile([]ImportTreeSpec{tree("a", "epic", ""), tree("b", "epic", "", "a", "a")})
			return err
		}, `import: spec "b": blocked_by names "a" twice`},
		{"tree: a blocked_by naming two records once each is accepted", func() error {
			_, err := ParseImportTreeFile([]ImportTreeSpec{tree("a", "epic", ""), tree("c", "epic", ""), tree("b", "epic", "", "a", "c")})
			return err
		}, ""},
		{"bulk: a task with no local_id blocked by the epic it sits under", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("e", "epic", ""), create("", "task", "e", "e")}, nil)
			return err
		}, `bulk: doc 1 blocked_by "e"` + sameEpic},
		{"bulk: an epic blocked by a task under it", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("e", "epic", "", "t"), create("t", "task", "e")}, nil)
			return err
		}, `bulk: doc 0 blocked_by "t"` + sameEpic},
		{"bulk: a task with no local_id blocked by a task beside it", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("e", "epic", ""), create("a", "task", "e"), create("", "chore", "e", "a")}, nil)
			return err
		}, `bulk: doc 2 blocked_by "a"` + sameEpic},
		{"bulk: two tasks under one existing epic, one blocked by the other", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("a", "task", "lit-epic"), create("", "bug", "lit-epic", "a")}, map[string]model.IssueType{"lit-epic": model.TypeEpic})
			return err
		}, `bulk: doc 1 blocked_by "a"` + sameEpic},
		{"bulk: two tasks under one existing task, which is no epic and the create refuses, are accepted", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("a", "task", "lit-task"), create("", "task", "lit-task", "a")}, map[string]model.IssueType{"lit-task": model.TypeTask})
			return err
		}, ""},
		{"bulk: two tasks under one id naming no issue, which the create refuses, are accepted", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("a", "task", "lit-gone"), create("", "task", "lit-gone", "a")}, map[string]model.IssueType{})
			return err
		}, ""},
		{"bulk: a task under an existing epic blocked by a task under another", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("a", "task", "lit-epic"), create("", "task", "lit-other", "a")}, map[string]model.IssueType{"lit-epic": model.TypeEpic, "lit-other": model.TypeEpic})
			return err
		}, ""},
		{"bulk: a task blocked by a real id, which the write judges, is accepted", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("e", "epic", ""), create("t", "task", "e", "lit-real-1")}, nil)
			return err
		}, ""},
		{"bulk: a create naming one real id twice in blocked_by", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("e", "epic", "", "lit-real-1", "lit-real-1")}, nil)
			return err
		}, `bulk: doc 0: blocked_by names "lit-real-1" twice`},
		{"bulk: an update with a doubled blocked_by is told update takes none", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{{ID: "lit-real-1", Title: str("t"), BlockedBy: []string{"lit-real-2", "lit-real-2"}}}, nil)
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

// One bad value is refused in one wording whichever document states it: a
// tree spec, a bulk create, or a bulk update names the document, then the key
// and the value, then what is allowed. Each refusal is a validation error, so
// it exits as the file being wrong.
func TestABadValueIsRefusedAlikeInEveryDocumentShape(t *testing.T) {
	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	const badType = `has invalid type "ghost": issue type must be task, feature, bug, chore, or epic`
	tree := ImportTreeSpec{LocalID: "e", Title: "t", IssueType: "epic", Topic: "core"}
	create := BulkIssueSpec{Title: str("t"), IssueType: str("epic"), Topic: str("core")}
	for _, c := range []struct {
		name string
		gate func() error
		want string
	}{
		{"tree spec type", func() error {
			spec := tree
			spec.IssueType = "ghost"
			_, err := ParseImportTreeFile([]ImportTreeSpec{spec})
			return err
		}, `import: spec "e": ` + badType},
		{"bulk create type", func() error {
			doc := create
			doc.IssueType = str("ghost")
			_, err := ParseBulkFile([]BulkIssueSpec{doc}, nil)
			return err
		}, `bulk: doc 0: ` + badType},
		{"bulk update type", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{{ID: "lit-x-1", IssueType: str("ghost")}}, nil)
			return err
		}, `bulk: doc 0 (id "lit-x-1"): ` + badType},
		{"tree spec priority", func() error {
			spec := tree
			spec.Priority = 9
			_, err := ParseImportTreeFile([]ImportTreeSpec{spec})
			return err
		}, `import: spec "e": has invalid priority 9: priority must be `},
		{"bulk create priority", func() error {
			doc := create
			doc.Priority = num(9)
			_, err := ParseBulkFile([]BulkIssueSpec{doc}, nil)
			return err
		}, `bulk: doc 0: has invalid priority 9: priority must be `},
		{"bulk update priority", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{{ID: "lit-x-1", Priority: num(9)}}, nil)
			return err
		}, `bulk: doc 0 (id "lit-x-1"): has invalid priority 9: priority must be `},
	} {
		err := c.gate()
		if err == nil {
			t.Errorf("%s: the gate admitted a bad value", c.name)
			continue
		}
		if !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("%s: refusal = %q, want it to start %q", c.name, err, c.want)
		}
		var validation model.ValidationError
		if !errors.As(err, &validation) {
			t.Errorf("%s: refusal %q is not a model.ValidationError", c.name, err)
		}
	}
}
