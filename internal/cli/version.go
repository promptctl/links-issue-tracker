package cli

import (
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/version"
)

// runVersion is the user-facing surface for the binary's identity: it prints
// the version, commit, build date, and supported schema range from version.Info.
//
// [LAW:one-source-of-truth] version.Info is the only data source.
func runVersion(stdout io.Writer, args []string) error {
	fs := newCobraFlagSet("version")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return UsageError{Message: "usage: lit version"}
	}

	info, err := version.Get()
	if err != nil {
		return err
	}

	ver := info.Version
	if info.IsDev {
		ver = "dev"
	}
	commit := info.Commit
	if commit == "" {
		commit = "unknown"
	}
	date := info.Date
	if date == "" {
		date = "unknown"
	}
	_, err = fmt.Fprintf(stdout,
		"lit %s (commit %s, built %s)\nschema versions supported: %d–%d\n",
		ver, commit, date, info.Schema.Min, info.Schema.Max,
	)
	return err
}
