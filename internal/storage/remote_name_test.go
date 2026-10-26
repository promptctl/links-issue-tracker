package storage

import (
	"errors"
	"strings"
	"testing"
)

func TestParseRemoteName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw     string
		want    string
		refusal string
	}{
		{raw: "origin", want: "origin"},
		{raw: " a ", want: "a"},
		{raw: "fork-2_old", want: "fork-2_old"},
		{raw: "", refusal: "remote is required"},
		{raw: "a/b", refusal: `remote "a/b" has "/" in its name`},
		{raw: "a/", refusal: `remote "a/" has "/" in its name`},
		{raw: "upstream.old", refusal: `remote "upstream.old" has "." in its name`},
		{raw: "my fork", refusal: `remote "my fork" has " " in its name`},
	} {
		got, err := ParseRemoteName(tc.raw)
		if tc.refusal == "" {
			if err != nil || got.String() != tc.want {
				t.Errorf("ParseRemoteName(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.refusal) {
			t.Errorf("ParseRemoteName(%q) error = %v, want one containing %q", tc.raw, err, tc.refusal)
		}
		// Only a name Dolt cannot hold is a refusal; an empty one names no remote.
		if refused := (RefusedRemoteError{}); errors.As(err, &refused) != (strings.TrimSpace(tc.raw) != "") {
			t.Errorf("ParseRemoteName(%q) error = %#v; want a RefusedRemoteError exactly when the name is not empty", tc.raw, err)
		}
	}
}
