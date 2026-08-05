package model

import (
	"errors"
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
