package store

import (
	"fmt"
	"strings"
)

// ParseSortSpecs turns a comma-separated sort expression (e.g.
// "rank:asc,updated_at:desc") into the ordered SortSpec list ListIssues consumes.
// A bare field ("rank") defaults to ascending; an unrecognized direction is
// rejected. Empty and whitespace-only expressions yield no specs.
//
// [LAW:one-source-of-truth] This is THE parser from sort expression to
// []SortSpec. Both the `--sort` flag and the `--query sort:` token route through
// here, so the two surfaces cannot drift on direction syntax or field naming.
// It lives beside SortSpec because the query package cannot import cli, where the
// caller originally kept a private copy.
func ParseSortSpecs(input string) ([]SortSpec, error) {
	out := make([]SortSpec, 0)
	for _, part := range strings.Split(input, ",") {
		spec := strings.TrimSpace(part)
		if spec == "" {
			continue
		}
		field := spec
		desc := false
		if strings.Contains(spec, ":") {
			chunks := strings.SplitN(spec, ":", 2)
			field = strings.TrimSpace(chunks[0])
			direction := strings.ToLower(strings.TrimSpace(chunks[1]))
			switch direction {
			case "asc":
				desc = false
			case "desc":
				desc = true
			default:
				// [LAW:no-silent-failure] An unrecognized direction is a typo, not
				// an implicit default — reject it so a bad sort never silently
				// reorders results. ValidationError maps to the same ExitValidation
				// the CLI's prior UnsupportedError used.
				return nil, ValidationError{Message: fmt.Sprintf("unsupported sort direction %q", direction)}
			}
		}
		out = append(out, SortSpec{Field: field, Desc: desc})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
