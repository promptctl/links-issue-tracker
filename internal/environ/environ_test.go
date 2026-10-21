package environ

import (
	"slices"
	"testing"
)

func TestLookupReadsAsTheOSPackageDoes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		goos   string
		list   []string
		key    string
		want   string
		wantOK bool
	}{
		{"set", "linux", []string{"A=1"}, "A", "1", true},
		{"unset", "linux", []string{"A=1"}, "B", "", false},
		{"set empty", "linux", []string{"A="}, "A", "", true},
		{"value holds =", "linux", []string{"A=x=y"}, "A", "x=y", true},
		{"last entry wins", "linux", []string{"A=1", "B=2", "A=3"}, "A", "3", true},
		{"case matters off windows", "darwin", []string{"Path=x"}, "PATH", "", false},
		{"case ignored on windows", "windows", []string{"Path=x"}, "PATH", "x", true},
		{"last entry wins across case on windows", "windows", []string{"PATH=x", "path=y"}, "Path", "y", true},
		{"windows drive entry", "windows", []string{"=C:=C:\\dir"}, "=C:", "C:\\dir", true},
		{"entry with no =", "linux", []string{"A"}, "A", "", true},
		{"empty entry", "linux", []string{""}, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := of(tc.list, tc.goos).Lookup(tc.key)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("Lookup(%q) = %q, %v; want %q, %v", tc.key, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestHomeDirReadsEachGOOSsVariable(t *testing.T) {
	t.Parallel()
	list := []string{"HOME=/h", "USERPROFILE=C:\\u", "home=/p9"}
	for goos, want := range map[string]string{"linux": "/h", "darwin": "/h", "windows": "C:\\u", "plan9": "/p9"} {
		if got, err := of(list, goos).HomeDir(); err != nil || got != want {
			t.Errorf("%s: HomeDir() = %q, %v; want %q", goos, got, err, want)
		}
		if got, err := of(nil, goos).HomeDir(); err == nil {
			t.Errorf("%s: HomeDir() of the empty environment = %q, want an error", goos, got)
		}
	}
}

func TestTempDirResolvesAsOSTempDir(t *testing.T) {
	t.Parallel()
	cases := []struct {
		goos string
		list []string
		want string
	}{
		{"linux", []string{"TMPDIR=/t"}, "/t"},
		{"linux", nil, "/tmp"},
		{"darwin", []string{"TMPDIR="}, "/tmp"},
		{"plan9", []string{"TMPDIR=/t"}, "/tmp"},
		{"windows", []string{"TEMP=C:\\b", "TMP=C:\\a"}, "C:\\a"},
		{"windows", []string{"TEMP=C:\\b", "USERPROFILE=C:\\u"}, "C:\\b"},
		{"windows", []string{"USERPROFILE=C:\\u", "SystemRoot=C:\\w"}, "C:\\u"},
		{"windows", []string{"SystemRoot=C:\\w"}, "C:\\w"},
	}
	for _, tc := range cases {
		if got, err := of(tc.list, tc.goos).TempDir(); err != nil || got != tc.want {
			t.Errorf("%s %q: TempDir() = %q, %v; want %q", tc.goos, tc.list, got, err, tc.want)
		}
	}
	if got, err := of(nil, "windows").TempDir(); err == nil {
		t.Errorf("windows: TempDir() of the empty environment = %q, want an error", got)
	}
}

func TestWithoutAndWithDeriveAChildsEnvironment(t *testing.T) {
	t.Parallel()
	parent := of([]string{"A=1", "B=2", "a=3"}, "linux")
	if got, want := parent.Without("A").With("C=4").List(), []string{"B=2", "a=3", "C=4"}; !slices.Equal(got, want) {
		t.Errorf("linux: List() = %q, want %q", got, want)
	}
	if got, want := of([]string{"A=1", "B=2", "a=3"}, "windows").Without("A").List(), []string{"B=2"}; !slices.Equal(got, want) {
		t.Errorf("windows: List() = %q, want %q", got, want)
	}
	if got, want := parent.List(), []string{"A=1", "B=2", "a=3"}; !slices.Equal(got, want) {
		t.Errorf("deriving changed the parent: List() = %q, want %q", got, want)
	}
}

// exec reads a nil Env as "inherit the process", so List never hands it one.
func TestListIsNeverNil(t *testing.T) {
	t.Parallel()
	for name, env := range map[string]Env{"zero": {}, "of nil": Of(nil), "emptied": Of([]string{"A=1"}).Without("A")} {
		if got := env.List(); got == nil || len(got) != 0 {
			t.Errorf("%s: List() = %#v, want an empty, non-nil list", name, got)
		}
	}
}

// A caller's list and the lists List hands out are copies: no one of them
// can change an Env another run holds.
func TestEnvSharesNoListWithItsCallers(t *testing.T) {
	t.Parallel()
	list := []string{"A=1"}
	env := Of(list)
	list[0] = "A=2"
	env.List()[0] = "A=3"
	if got := env.Get("A"); got != "1" {
		t.Errorf("Get(A) = %q after its caller's lists changed, want %q", got, "1")
	}
}
