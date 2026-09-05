package model

import (
	"errors"
	"slices"
	"strings"
)

// NormalizeLabel returns a label's canonical form: lowercased and trimmed.
// Empty (after trimming) and comma-carrying labels are rejected — commas are
// reserved as the list separator on label input surfaces.
// [LAW:single-enforcer] This is the one definition of canonical label form;
// the store persists it and every matching surface (e.g. workflow label
// activation) compares against it. Callsites must not re-derive it locally.
func NormalizeLabel(label string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(label))
	if normalized == "" {
		return "", errors.New("label is required")
	}
	if strings.Contains(normalized, ",") {
		return "", errors.New("label cannot contain commas")
	}
	return normalized, nil
}

// CanonicalizeLabels reduces an authored list to its stored form: every name
// in canonical form, duplicates dropped, sorted. Order and duplication are not
// label-set facts, so two authorings of one set canonicalize to one list and
// compare equal.
// [LAW:one-source-of-truth] The set-level form belongs beside the name-level
// one. Both engines derived this locally from NormalizeLabel and their copies
// were the same algorithm typed twice — which is a divergence waiting on
// whichever copy someone edits first.
func CanonicalizeLabels(labels []string) ([]string, error) {
	out := make([]string, 0, len(labels))
	seen := map[string]struct{}{}
	for _, label := range labels {
		name, err := NormalizeLabel(label)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}
