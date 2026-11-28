package config

import (
	"errors"
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// FileVersion is one version of the project config file, as the checkout or
// its HEAD commit holds it: its text, or no file.
type FileVersion struct {
	Present bool
	Content string
}

// ProjectFile is the checkout's project config file in the two versions an
// apply compares: the working file and the one HEAD holds.
type ProjectFile struct {
	Path      string
	Working   FileVersion
	Committed FileVersion
}

// Standing is how the checkout's project config file stands against the
// config in force on its store.
type Standing string

const (
	// StandingInForce: the committed file is the config in force, or there
	// is no file and nothing has been applied, so lit runs on its defaults.
	StandingInForce Standing = "in_force"
	// StandingNotApplied: the file is committed and is not the config in
	// force; `lit config apply` puts it in force.
	StandingNotApplied Standing = "not_applied"
	// StandingUncommitted: the working file differs from HEAD's, a file HEAD
	// lacks or a deletion included. It has no effect, and cannot be applied
	// until it is committed.
	StandingUncommitted Standing = "uncommitted"
	// StandingNoFile: the checkout has no file and HEAD none, and a config
	// was applied. The applied config stays in force; applying an empty file
	// returns the store to the defaults.
	StandingNoFile Standing = "no_file"
)

// StandingOf compares the file's two versions with the config in force.
// [LAW:effects-at-boundaries] pure: the reads are the caller's.
func StandingOf(file ProjectFile, applies []model.ConfigApply) Standing {
	inForce, applied := model.ConfigInForce(applies)
	switch {
	case file.Working != file.Committed:
		return StandingUncommitted
	case !file.Working.Present && applied:
		return StandingNoFile
	case !file.Working.Present:
		return StandingInForce
	case applied && inForce.Content == file.Working.Content:
		return StandingInForce
	default:
		return StandingNotApplied
	}
}

// Applicable is the content `lit config apply` puts in force, or every reason
// it refuses: no file, a file HEAD does not hold, a working file that differs
// from HEAD's, and a config CheckProject refuses. Only the committed file is
// applied, so what every clone runs on is a version anyone can check out.
func (f ProjectFile) Applicable() (string, error) {
	var faults []error
	if !f.Working.Present {
		faults = append(faults, fmt.Errorf("there is no project config file at %s", f.Path))
	}
	if !f.Committed.Present {
		faults = append(faults, fmt.Errorf("%s is not committed: only a committed file is applied, so commit it first", f.Path))
	}
	if f.Working.Present && f.Committed.Present && f.Working.Content != f.Committed.Content {
		faults = append(faults, fmt.Errorf("%s has uncommitted changes: only the committed file is applied, so commit them first", f.Path))
	}
	if f.Working.Present {
		if err := CheckProject([]byte(f.Working.Content)); err != nil {
			faults = append(faults, fmt.Errorf("%s is not a config lit can run on: %w", f.Path, err))
		}
	}
	if err := errors.Join(faults...); err != nil {
		return "", err
	}
	return f.Committed.Content, nil
}
