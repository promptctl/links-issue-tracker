package environ

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/dolthub/dolt/go/libraries/utils/gitcmd"
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

// Every child a constructor builds runs in the Env's own list, and an empty
// Env still hands over a non-nil one, so no child inherits the process.
func TestConstructorsRunTheChildInTheEnv(t *testing.T) {
	t.Parallel()
	for name, env := range map[string]Env{"set": Of([]string{"A=1"}), "empty": {}} {
		want := env.List()
		ctx := context.Background()
		if got := env.Command("true").Env; got == nil || !slices.Equal(got, want) {
			t.Errorf("%s: Command(...).Env = %#v, want %#v", name, got, want)
		}
		if got := env.CommandContext(ctx, "true").Env; got == nil || !slices.Equal(got, want) {
			t.Errorf("%s: CommandContext(...).Env = %#v, want %#v", name, got, want)
		}
	}
}

// Git overrides whatever Env its spec carried, nil included: git reads
// GIT_EDITOR from the Env, not from the spec or the process.
func TestGitRunsInTheEnv(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_EDITOR", "from-process")
	env := Of([]string{"GIT_EDITOR=from-env"})
	for name, spec := range map[string]gitcmd.Spec{"nil Env": {Dir: t.TempDir()}, "other Env": {Dir: t.TempDir(), Env: []string{"GIT_EDITOR=from-spec"}}} {
		out, err := env.Git(context.Background(), spec, "var", "GIT_EDITOR").Output()
		if err != nil {
			t.Fatalf("%s: git var GIT_EDITOR: %v", name, err)
		}
		if got := strings.TrimSpace(string(out)); got != "from-env" {
			t.Errorf("%s: git saw GIT_EDITOR=%q, want %q", name, got, "from-env")
		}
	}
}
