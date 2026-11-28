package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/selector"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// flagAtom is a filter flag read as the atom it stands for: path op the flag's
// values. values are the flag's occurrences, each a comma list when split and
// one value whole otherwise, as search text and stamps are. each makes every
// value its own atom, so all must hold rather than any. given is whether a
// flag that takes no value was given; one that takes values says so by having
// them.
type flagAtom struct {
	flag   string
	path   string
	op     selector.Op
	values []string
	split  bool
	each   bool
	given  bool
}

// flagSelector reads the filter flags as one selector, every atom built
// through the parsing and refusals the same atom in --query gets, so a flag
// and the atom it stands for cannot select two different things. A blank
// occurrence is refused rather than dropped: a dropped value would widen or
// unfilter the listing while it keeps the shape of a real answer.
// [LAW:single-enforcer] [LAW:no-silent-failure]
func flagSelector(now time.Time, flags []flagAtom) (storage.Selector, error) {
	var parts []storage.Selector
	for _, f := range flags {
		if len(f.values) == 0 && !f.given {
			continue
		}
		refusal := fmt.Sprintf("--%s needs a value in every slot, e.g. --%s <value>", f.flag, f.flag)
		values, err := occurrenceValues(refusal, f.values, f.split)
		if err != nil {
			return storage.Selector{}, err
		}
		groups := [][]string{values}
		if f.op == selector.Present || f.op == selector.Absent {
			groups = [][]string{nil}
		} else if f.each {
			groups = nil
			for _, v := range values {
				groups = append(groups, []string{v})
			}
		}
		for _, group := range groups {
			atom, err := storage.ParseAtom(now, f.path, f.op, group...)
			if err != nil {
				return storage.Selector{}, fmt.Errorf("parse --%s: %w", f.flag, err)
			}
			parts = append(parts, atom)
		}
	}
	return storage.And(parts...), nil
}

// occurrenceValues is the values a flag's occurrences state, each trimmed: a
// split occurrence is a comma list, a whole one a single value.
func occurrenceValues(refusal string, occurrences []string, split bool) ([]string, error) {
	if split {
		return storage.ParseNames(refusal, occurrences...)
	}
	values := make([]string, 0, len(occurrences))
	for _, occurrence := range occurrences {
		value := strings.TrimSpace(occurrence)
		if value == "" {
			return nil, model.ValidationError{Message: refusal}
		}
		values = append(values, value)
	}
	return values, nil
}

// occurrence is the one value a single-valued flag was given, and none when
// it was not.
func occurrence(given bool, value string) []string {
	if !given {
		return nil
	}
	return []string{value}
}

// presenceOf is the presence test a boolean flag states: the path reads a
// value, or reads none.
func presenceOf(present bool) selector.Op {
	if present {
		return selector.Present
	}
	return selector.Absent
}

// activeByDefault is what a listing selects when it says nothing about the
// state: the work still to do. A status atom says what it wants, and so does a
// resolution atom, since only a closed issue has a resolution: either one
// leaves the selector as written. [LAW:no-silent-failure] Without the
// resolution rule, a closed-only selector would meet the default and answer
// empty.
func activeByDefault(sel storage.Selector) storage.Selector {
	if sel.Starts(storage.IssueStatus.Field) || sel.Starts(storage.IssueResolution) {
		return sel
	}
	return storage.And(storage.IssueStatus.Is(model.StateOpen, model.StateInProgress), sel)
}

// requireParents refuses a listing under a parent it cannot list under: one no
// issue has, which is a mistyped id rather than a parent with no children, and
// one outside the issues the read admits, whose children the selector would
// find no parent for. It reads every named parent once, in every retention,
// and judges each by the selection's own admission rule.
// [LAW:no-silent-failure]
func requireParents(ctx context.Context, st storage.IssueReader, selection storage.Selection, parentIDs []model.ID) error {
	if len(parentIDs) == 0 {
		return nil
	}
	found, err := st.ListIssues(ctx, storage.Listing{Selection: storage.Selection{
		Selector:        storage.IssueID.Is(parentIDs...),
		IncludeArchived: true,
		IncludeDeleted:  true,
	}})
	if err != nil {
		return err
	}
	byID := make(map[model.ID]model.Issue, len(found))
	for _, issue := range found {
		byID[issue.ID] = issue
	}
	for _, id := range parentIDs {
		issue, ok := byID[id]
		if !ok {
			return storage.NotFoundError{Entity: "issue", ID: id}
		}
		if !selection.Admits(issue.Retention()) {
			return model.ValidationError{Message: fmt.Sprintf("parent %s is %s; --%s lists under it", id, retentionWord(issue.Retention()), retentionFlag(issue.Retention()))}
		}
	}
	return nil
}

func retentionWord(r model.Retention) string {
	if _, ok := r.(model.Deleted); ok {
		return "deleted"
	}
	return "archived"
}

func retentionFlag(r model.Retention) string {
	return "include-" + retentionWord(r)
}

// parseLimit reads a listing cap where the value enters. Zero is the uncapped
// default; a negative limit is always a typo, which every reader of `<= 0` as
// uncapped would silently turn into everything. [LAW:no-silent-failure]
func parseLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, model.ValidationError{Message: fmt.Sprintf("limit must be non-negative, got %d", limit)}
	}
	return limit, nil
}

// listReference is the whole contract of --query on a listing: the selector
// reference the field table generates, and the two rules the listing adds.
func listReference() string {
	return storage.SelectorReference() + `
Archived and deleted issues exist for the selector only with --include-archived
and --include-deleted, at every step of every path. Unless an atom reads the
issue's own status or resolution, the listing selects open and in_progress
issues.
`
}
