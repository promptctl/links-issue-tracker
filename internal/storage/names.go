package storage

import (
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// ParseNames reads a set of names however the caller spelled it: each input
// is a comma list, and the inputs union, so a repeated flag widens one set.
// Every fragment must name something: a blank one (`--ids ""`, `--ids a,`) is
// refused with the caller's refusal rather than dropped, because a dropped
// name narrows or unfilters the listing while it keeps the shape of a real
// answer. [LAW:single-enforcer] The comma rule for a flag's set lives here;
// the caller owns only the wording of its refusal. [LAW:no-silent-failure]
func ParseNames(refusal string, inputs ...string) ([]string, error) {
	var out []string
	for _, input := range inputs {
		for _, field := range strings.Split(input, ",") {
			name := strings.TrimSpace(field)
			if name == "" {
				return nil, model.ValidationError{Message: refusal}
			}
			out = append(out, name)
		}
	}
	return out, nil
}
