package main

import (
	"fmt"
	"io"
	"strings"
)

// bundleRule visually separates one module's section from the next in the
// flat THIRD_PARTY_LICENSES text file.
const bundleRule = "================================================================================"

// WriteBundle renders the third-party attribution bundle: one section per
// linked module, holding its module coordinates, classified license name, and
// the full verbatim text of its license file. Section order follows entries
// exactly as given — LinkedModules already sorted it by module path, so this
// function adds no ordering logic of its own. [LAW:one-source-of-truth]
// entries carries the one canonical module order; this function doesn't
// re-derive or re-sort it.
func WriteBundle(w io.Writer, entries []Entry) error {
	for _, e := range entries {
		_, err := fmt.Fprintf(w, "%s\n%s %s\nLicense: %s\n%s\n\n%s\n\n",
			bundleRule,
			e.Module.Path, e.Module.Version,
			e.LicenseName,
			bundleRule,
			strings.TrimRight(e.Text, "\n"),
		)
		if err != nil {
			return fmt.Errorf("write bundle entry for %s: %w", e.Module.Path, err)
		}
	}
	return nil
}
