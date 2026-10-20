package storage

import (
	"errors"
	"slices"
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
		return BulkIssueSpec{LocalID: localID, Title: str("title"), IssueType: str(issueType), Topic: str("core"), Parent: ptr(parent), BlockedBy: ptr(blockedBy)}
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
		{"bulk: two update documents each under the other", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{{ID: "lit-a", Parent: ptr("lit-b")}, {ID: "lit-b", Parent: ptr("lit-a")}}, nil)
			return err
		}, `bulk: cycle detected involving "lit-a"`},
		{"bulk: a create under an update document the file puts under it", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("c", "epic", "lit-x"), {ID: "lit-x", Parent: ptr("c")}}, nil)
			return err
		}, `bulk: cycle detected involving "c"`},
		{"bulk: an update document under itself", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{{ID: "lit-x", Parent: ptr("lit-x")}}, nil)
			return err
		}, `bulk: cycle detected involving "lit-x"`},
		{"bulk: an update document under a create that is no epic", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("t", "task", "lit-e"), {ID: "lit-x", Parent: ptr("t")}}, nil)
			return err
		}, "bulk: doc 1: " + RejectParentThatIsNotAContainer("t", model.TypeTask).Error()},
		{"bulk: a task blocked by an epic it sits under through an epic the file moves there", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("e", "epic", ""), {ID: "lit-x", Parent: ptr("e")}, create("", "task", "lit-x", "e")}, map[string]model.IssueType{"lit-x": model.TypeEpic})
			return err
		}, `bulk: doc 2 blocked_by "e"` + sameEpic},
		{"bulk: the same task under an epic the file leaves where it is, is accepted", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{create("e", "epic", ""), create("", "task", "lit-x", "e")}, map[string]model.IssueType{"lit-x": model.TypeEpic})
			return err
		}, ""},
		{"bulk: an update document that only reparents is accepted", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{{ID: "lit-x", Parent: ptr("lit-e")}, {ID: "lit-y", Parent: ptr("")}}, nil)
			return err
		}, ""},
		{"bulk: a reason on an update document that only reparents", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{{ID: "lit-x", Parent: ptr("lit-e"), Reason: "regroom"}}, nil)
			return err
		}, `bulk: doc 0 (id "lit-x") sets reason but changes no field; reason annotates a field change, and parent, blocked_by and rank write none`},
		{"bulk: an update naming one blocker twice is refused, as a create is", func() error {
			_, err := ParseBulkFile([]BulkIssueSpec{{ID: "lit-real-1", Title: str("t"), BlockedBy: ptr([]string{"lit-real-2", "lit-real-2"})}}, nil)
			return err
		}, `bulk: doc 0: blocked_by names "lit-real-2" twice`},
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

// A bulk file's ordering is every document stating `rank: top`, creates and
// updates alike, in the order the file lists them — not the write order its
// references force on the creates. An update document may state rank alone,
// and then writes no field change; one stating neither a field nor rank, a
// reason with rank alone, or a rank other than top, is refused.
func TestABulkFileOrderingIsItsRankTopDocumentsInFileOrder(t *testing.T) {
	str := func(s string) *string { return &s }
	top := str("top")
	file, err := ParseBulkFile([]BulkIssueSpec{
		{LocalID: "leaf", Title: str("leaf"), IssueType: str("task"), Topic: str("core"), Parent: ptr("root"), Rank: top},
		{ID: "lit-a-1", Rank: top},
		{ID: "lit-b-2", Title: str("retitled"), Rank: top},
		{LocalID: "root", Title: str("root"), IssueType: str("epic"), Topic: str("core"), Rank: top},
		{ID: "lit-c-3", Title: str("unranked")},
	}, nil)
	if err != nil {
		t.Fatalf("ParseBulkFile error = %v", err)
	}
	// root is written before the leaf filed under it, so the ids the write
	// mints arrive in the reverse of the file's order.
	created := []string{"created-root", "created-leaf"}
	if got, want := file.RankedIDs(created), []string{"created-leaf", "lit-a-1", "lit-b-2", "created-root"}; !slices.Equal(got, want) {
		t.Errorf("RankedIDs = %v, want %v", got, want)
	}
	var updated []string
	for _, u := range file.Updates {
		updated = append(updated, u.ID)
	}
	if want := []string{"lit-b-2", "lit-c-3"}; !slices.Equal(updated, want) {
		t.Errorf("updates = %v, want %v: a document stating only rank writes no field change", updated, want)
	}

	for _, refused := range []struct {
		name string
		doc  BulkIssueSpec
		want string
	}{
		{"a rank other than top", BulkIssueSpec{ID: "lit-a-1", Rank: str("bottom")}, `bulk: doc 0: rank "bottom" is not a placement; the one placement is top`},
		{"an update stating neither a field nor rank", BulkIssueSpec{ID: "lit-a-1"}, `bulk: doc 0 (id "lit-a-1") has no fields to update, no parent, no blocked_by and no rank`},
		{"a reason on an update stating only rank", BulkIssueSpec{ID: "lit-a-1", Rank: top, Reason: "reprioritized"}, `bulk: doc 0 (id "lit-a-1") sets reason but changes no field`},
	} {
		if _, err := ParseBulkFile([]BulkIssueSpec{refused.doc}, nil); err == nil || !strings.HasPrefix(err.Error(), refused.want) {
			t.Errorf("%s: err = %v, want it to start %q", refused.name, err, refused.want)
		}
	}
	if file, err := ParseBulkFile([]BulkIssueSpec{{ID: "lit-a-1", Title: str("t")}}, nil); err != nil || len(file.RankedIDs(nil)) != 0 {
		t.Errorf("a file stating no rank: ordering %v, err %v; want an empty ordering", file.RankedIDs(nil), err)
	}
}

func ptr[T any](v T) *T { return &v }

// The engines read the type of every issue the gate places: each parent a
// file names and each update document that states a parent, cleared or not.
// An update document stating none is placed nowhere, so its type is not read.
func TestBulkExistingRefsNamesEveryIssueTheFilePlaces(t *testing.T) {
	got := BulkExistingRefs([]BulkIssueSpec{
		{LocalID: "c", Title: ptr("c"), Parent: ptr("lit-e")},
		{ID: "lit-x", Parent: ptr("c")},
		{ID: "lit-y", Parent: ptr("")},
		{ID: "lit-z", Title: ptr("t")},
	})
	if want := []string{"lit-e", "c", "lit-x", "lit-y"}; !slices.Equal(got, want) {
		t.Errorf("BulkExistingRefs = %v, want %v", got, want)
	}
}
