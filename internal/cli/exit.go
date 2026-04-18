package cli

import (
	"errors"
	"strings"

	"github.com/bmf/links-issue-tracker/internal/store"
)

const (
	ExitOK         = 0
	ExitGeneric    = 1
	ExitUsage      = 2
	ExitValidation = 3
	ExitNotFound   = 4
	ExitConflict   = 5
	ExitCorruption = 7
)

func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var notFound store.NotFoundError
	if errors.As(err, &notFound) {
		return ExitNotFound
	}
	var mergeConflict MergeConflictError
	if errors.As(err, &mergeConflict) {
		return ExitConflict
	}
	var corruption CorruptionError
	if errors.As(err, &corruption) {
		return ExitCorruption
	}
	var beadsRequired BeadsMigrationRequiredError
	if errors.As(err, &beadsRequired) {
		return ExitValidation
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.HasPrefix(message, "usage:"),
		strings.HasPrefix(message, "unknown flag"):
		return ExitUsage
	case strings.Contains(message, "required"),
		strings.Contains(message, "must be"),
		strings.Contains(message, "unsupported"),
		strings.Contains(message, "--output is no longer supported"),
		strings.Contains(message, "unknown command"):
		return ExitValidation
	case strings.Contains(message, "conflict"):
		return ExitConflict
	default:
		return ExitGeneric
	}
}
