package cli

import (
	"fmt"
	"io"

	"github.com/promptctl/links-issue-tracker/internal/version"
)

// runVersion is the user-facing surface for the binary's identity. Human and
// JSON modes are produced from the same version.Info — no JSON-only fields,
// no text-only fields. The JSON form is the documented contract downstream
// tools read; consumers MUST NOT parse the text form.
//
// [LAW:one-source-of-truth] version.Info is the only data source. Both surfaces
// (human text + JSON) project from it.
// [LAW:single-enforcer] printValue chooses the output mode (--json flag OR
// detected machine output). The text formatter handles only presentation —
// no field is derived in this function that isn't already on the struct.
func runVersion(stdout io.Writer, args []string) error {
	fs := newCobraFlagSet("version")
	jsonOut := fs.Bool("json", false, "Output JSON")
	if err := parseFlagSet(fs, args, stdout); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: lit version [--json]")
	}

	info, err := version.Get()
	if err != nil {
		return err
	}

	return printValue(stdout, info, *jsonOut, func(w io.Writer, v any) error {
		i := v.(version.Info)
		ver := i.Version
		if i.IsDev {
			ver = "dev"
		}
		commit := i.Commit
		if commit == "" {
			commit = "unknown"
		}
		date := i.Date
		if date == "" {
			date = "unknown"
		}
		_, err := fmt.Fprintf(w,
			"lit %s (commit %s, built %s)\nschema versions supported: %d–%d\n",
			ver, commit, date, i.Schema.Min, i.Schema.Max,
		)
		return err
	})
}
