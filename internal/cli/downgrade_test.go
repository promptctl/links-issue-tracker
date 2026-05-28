package cli

import (
	"strings"
	"testing"
)

func TestNormalizeDowngradeTag(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{"v0.4.1", "v0.4.1", ""},
		{"0.4.1", "v0.4.1", ""},
		{" v0.4.1 ", "v0.4.1", ""},
		{"", "", "required"},
		{"v0.4.1/etc", "", "not a valid"},
		{"v0.4..1", "", "not a valid"},
		{"v0 .4.1", "", "not a valid"},
	}
	for _, c := range cases {
		got, err := normalizeDowngradeTag(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("normalizeDowngradeTag(%q) err = %v; want contains %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeDowngradeTag(%q) err = %v; want nil", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeDowngradeTag(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
