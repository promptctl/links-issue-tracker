package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model"
	"github.com/bmf/links-issue-tracker/internal/store"
)

type ParseResult struct {
	Filter store.ListIssuesFilter
}

func Parse(input string) (ParseResult, error) {
	terms, err := tokenize(strings.TrimSpace(input))
	if err != nil {
		return ParseResult{}, err
	}
	filter := store.ListIssuesFilter{}
	for _, term := range terms {
		if err := applyTerm(&filter, term); err != nil {
			return ParseResult{}, err
		}
	}
	return ParseResult{Filter: filter}, nil
}

func Merge(base store.ListIssuesFilter, incoming store.ListIssuesFilter) (store.ListIssuesFilter, error) {
	filter := base
	normalizedBase, err := normalizeQueryStatuses(filter.Statuses)
	if err != nil {
		return store.ListIssuesFilter{}, err
	}
	normalizedIncoming, err := normalizeQueryStatuses(incoming.Statuses)
	if err != nil {
		return store.ListIssuesFilter{}, err
	}
	filter.Statuses = mergeStateSlice(normalizedBase, normalizedIncoming)
	filter.IssueTypes = mergeSlice(filter.IssueTypes, incoming.IssueTypes)
	filter.Assignees = mergeSlice(filter.Assignees, incoming.Assignees)
	filter.SearchTerms = append(filter.SearchTerms, incoming.SearchTerms...)
	filter.IDs = append(filter.IDs, incoming.IDs...)
	filter.LabelsAll = append(filter.LabelsAll, incoming.LabelsAll...)
	if err := mergeIntPointer("priority-min", &filter.PriorityMin, incoming.PriorityMin); err != nil {
		return store.ListIssuesFilter{}, err
	}
	if err := mergeIntPointer("priority-max", &filter.PriorityMax, incoming.PriorityMax); err != nil {
		return store.ListIssuesFilter{}, err
	}
	if err := mergeBoolPointer("has-comments", &filter.HasComments, incoming.HasComments); err != nil {
		return store.ListIssuesFilter{}, err
	}
	if err := mergeTimePointer("updated-after", &filter.UpdatedAfter, incoming.UpdatedAfter); err != nil {
		return store.ListIssuesFilter{}, err
	}
	if err := mergeTimePointer("updated-before", &filter.UpdatedBefore, incoming.UpdatedBefore); err != nil {
		return store.ListIssuesFilter{}, err
	}
	if incoming.Limit > 0 {
		filter.Limit = incoming.Limit
	}
	return filter, validateFilter(filter)
}

func applyTerm(filter *store.ListIssuesFilter, term string) error {
	switch {
	case strings.HasPrefix(term, "status:"):
		parsed, err := model.ParseState(strings.TrimPrefix(term, "status:"))
		if err != nil {
			return err
		}
		filter.Statuses = append(filter.Statuses, parsed)
		return nil
	case strings.HasPrefix(term, "type:"):
		t := strings.TrimSpace(strings.TrimPrefix(term, "type:"))
		if t == "" {
			return nil
		}
		filter.IssueTypes = append(filter.IssueTypes, t)
		return nil
	case strings.HasPrefix(term, "assignee:"):
		filter.Assignees = append(filter.Assignees, strings.TrimSpace(strings.TrimPrefix(term, "assignee:")))
		return nil
	case strings.HasPrefix(term, "id:"):
		filter.IDs = append(filter.IDs, strings.TrimSpace(strings.TrimPrefix(term, "id:")))
		return nil
	case strings.HasPrefix(term, "label:"):
		filter.LabelsAll = append(filter.LabelsAll, strings.TrimSpace(strings.TrimPrefix(term, "label:")))
		return nil
	case strings.HasPrefix(term, "has:"):
		switch strings.TrimSpace(strings.TrimPrefix(term, "has:")) {
		case "comments":
			value := true
			return mergeBoolPointer("has-comments", &filter.HasComments, &value)
		default:
			return fmt.Errorf("unsupported has: filter %q", term)
		}
	case strings.HasPrefix(term, "priority"):
		return applyPriority(filter, strings.TrimPrefix(term, "priority"))
	case strings.HasPrefix(term, "p:") || strings.HasPrefix(term, "p>") || strings.HasPrefix(term, "p<"):
		return applyPriority(filter, strings.TrimPrefix(term, "p"))
	case strings.HasPrefix(term, "updated"):
		return applyTimeTerm(filter, strings.TrimPrefix(term, "updated"))
	default:
		filter.SearchTerms = append(filter.SearchTerms, term)
		return nil
	}
}

func normalizeQueryStatuses(statuses []model.State) ([]model.State, error) {
	result := make([]model.State, 0, len(statuses))
	for _, s := range statuses {
		parsed, err := model.ParseState(string(s))
		if err != nil {
			return nil, err
		}
		result = append(result, parsed)
	}
	return result, nil
}

func mergeStateSlice(base, incoming []model.State) []model.State {
	if len(incoming) == 0 {
		return base
	}
	seen := make(map[model.State]bool, len(base))
	for _, v := range base {
		seen[v] = true
	}
	result := append([]model.State{}, base...)
	for _, v := range incoming {
		if !seen[v] {
			result = append(result, v)
			seen[v] = true
		}
	}
	return result
}

func mergeSlice(base, incoming []string) []string {
	if len(incoming) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base))
	for _, v := range base {
		seen[v] = true
	}
	result := append([]string{}, base...)
	for _, v := range incoming {
		if !seen[v] {
			result = append(result, v)
			seen[v] = true
		}
	}
	return result
}

func applyPriority(filter *store.ListIssuesFilter, expr string) error {
	comparator, value, err := splitComparator(expr)
	if err != nil {
		return fmt.Errorf("parse priority term %q: %w", "priority"+expr, err)
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("priority must be an integer")
	}
	switch comparator {
	case ":":
		if err := mergeIntPointer("priority-min", &filter.PriorityMin, &parsed); err != nil {
			return err
		}
		return mergeIntPointer("priority-max", &filter.PriorityMax, &parsed)
	case ">=", ">":
		return mergeIntPointer("priority-min", &filter.PriorityMin, &parsed)
	case "<=", "<":
		return mergeIntPointer("priority-max", &filter.PriorityMax, &parsed)
	default:
		return fmt.Errorf("unsupported priority comparator %q", comparator)
	}
}

func applyTimeTerm(filter *store.ListIssuesFilter, expr string) error {
	comparator, value, err := splitComparator(expr)
	if err != nil {
		return fmt.Errorf("parse updated term %q: %w", "updated"+expr, err)
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return fmt.Errorf("updated timestamp must be RFC3339")
		}
	}
	switch comparator {
	case ">=", ">":
		return mergeTimePointer("updated-after", &filter.UpdatedAfter, &parsed)
	case "<=", "<":
		return mergeTimePointer("updated-before", &filter.UpdatedBefore, &parsed)
	default:
		return fmt.Errorf("updated supports only >=, >, <=, <")
	}
}

func splitComparator(expr string) (string, string, error) {
	value := strings.TrimSpace(expr)
	for _, comparator := range []string{">=", "<=", ">", "<", ":"} {
		if strings.HasPrefix(value, comparator) {
			payload := strings.TrimSpace(strings.TrimPrefix(value, comparator))
			if payload == "" {
				return "", "", fmt.Errorf("missing value")
			}
			return comparator, payload, nil
		}
	}
	return "", "", fmt.Errorf("missing comparator")
}

func tokenize(input string) ([]string, error) {
	if input == "" {
		return nil, nil
	}
	var out []string
	var current strings.Builder
	var quote rune
	for _, r := range input {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
		case r == ' ' || r == '\n' || r == '\t':
			if current.Len() > 0 {
				out = append(out, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in query")
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out, nil
}

func validateFilter(filter store.ListIssuesFilter) error {
	if filter.PriorityMin != nil && filter.PriorityMax != nil && *filter.PriorityMin > *filter.PriorityMax {
		return fmt.Errorf("priority-min cannot be greater than priority-max")
	}
	if filter.UpdatedAfter != nil && filter.UpdatedBefore != nil && filter.UpdatedAfter.After(*filter.UpdatedBefore) {
		return fmt.Errorf("updated-after cannot be greater than updated-before")
	}
	return nil
}


func mergeIntPointer(name string, dst **int, incoming *int) error {
	if incoming == nil {
		return nil
	}
	if *dst != nil && **dst != *incoming {
		return fmt.Errorf("conflicting %s filters %d and %d", name, **dst, *incoming)
	}
	if *dst == nil {
		value := *incoming
		*dst = &value
	}
	return nil
}

func mergeBoolPointer(name string, dst **bool, incoming *bool) error {
	if incoming == nil {
		return nil
	}
	if *dst != nil && **dst != *incoming {
		return fmt.Errorf("conflicting %s filters", name)
	}
	if *dst == nil {
		value := *incoming
		*dst = &value
	}
	return nil
}

func mergeTimePointer(name string, dst **time.Time, incoming *time.Time) error {
	if incoming == nil {
		return nil
	}
	if *dst != nil && !(*dst).Equal(*incoming) {
		return fmt.Errorf("conflicting %s filters %s and %s", name, (*dst).Format(time.RFC3339), incoming.Format(time.RFC3339))
	}
	if *dst == nil {
		value := incoming.UTC()
		*dst = &value
	}
	return nil
}
