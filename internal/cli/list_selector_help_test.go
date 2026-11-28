package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/selector"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// The help of both listing surfaces is the whole --query contract: every field
// a selector can read is named there, from the one table the parser reads.
func TestListHelpNamesEverySelectorField(t *testing.T) {
	t.Parallel()
	for _, surface := range []listSurface{lsSurface, childrenSurface} {
		var out bytes.Buffer
		if err := runList(underInvocation(context.Background()), dirInvocation(t, t.TempDir(), &out, &out, nil), surface, []string{"--help"}); !errors.Is(err, errHelpHandled) {
			t.Fatalf("%s --help error = %v, want errHelpHandled", surface.name, err)
		}
		help := out.String()
		for _, field := range storage.IssueTable.Fields() {
			if !strings.Contains(help, "\n  "+field.Name()+" ") {
				t.Errorf("%s --help does not name the field %s:\n%s", surface.name, field.Name(), help)
			}
		}
	}
}

// Every field the help names answers an atom on the listing.
func TestListQueryReadsEveryField(t *testing.T) {
	t.Parallel()
	h := newReadyTestHarness(t)
	for _, field := range storage.IssueTable.Fields() {
		atom := field.Name()
		if field.Kind() == selector.Search {
			atom += "~x"
		}
		runLs(t, h.ap, "--query", atom)
	}
}
