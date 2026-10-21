package environ

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

// Only an Env no one built is zero: the empty environment is one a run can
// be given.
func TestIsZeroIsOnlyTheEnvNoOneBuilt(t *testing.T) {
	t.Parallel()
	if !(Env{}).IsZero() || !(Env{}).With("A=1").IsZero() {
		t.Error("the zero Env, or one derived from it, is not IsZero")
	}
	for name, env := range map[string]Env{"of nil": Of(nil), "of a list": Of([]string{"A=1"}), "emptied": Of([]string{"A=1"}).Without("A")} {
		if env.IsZero() {
			t.Errorf("%s: IsZero() = true, want an environment a run can be given", name)
		}
	}
}

// Every child a constructor builds runs in the Env's own list, and an empty
// Env still hands over a non-nil one, so no child inherits the process. One
// started in the caller's directory gets the list as it is.
func TestConstructorsRunTheChildInTheEnv(t *testing.T) {
	t.Parallel()
	for name, env := range map[string]Env{"set": Of([]string{"A=1"}), "empty": {}} {
		want := env.List()
		ctx := context.Background()
		if got := env.Command("", "true").Env; got == nil || !slices.Equal(got, want) {
			t.Errorf("%s: Command(...).Env = %#v, want %#v", name, got, want)
		}
		if got := env.CommandContext(ctx, "", "true").Env; got == nil || !slices.Equal(got, want) {
			t.Errorf("%s: CommandContext(...).Env = %#v, want %#v", name, got, want)
		}
	}
}

// A child started in a directory runs there with PWD naming it, as os/exec
// sets it for a child that inherits the process, whatever PWD the Env holds;
// windows and plan9 get none, as os/exec gives them none.
func TestConstructorsGiveAChildItsDirectoryAsPWD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for goos, want := range map[string][]string{
		"linux":   {"PWD=/lit/ran/here", "PWD=/child/dir"},
		"darwin":  {"PWD=/lit/ran/here", "PWD=/child/dir"},
		"windows": {"PWD=/lit/ran/here"},
		"plan9":   {"PWD=/lit/ran/here"},
	} {
		env := of([]string{"PWD=/lit/ran/here"}, goos)
		cmd, ctxCmd := env.Command("/child/dir/", "true"), env.CommandContext(ctx, "/child/dir/", "true")
		for name, got := range map[string]*exec.Cmd{"Command": cmd, "CommandContext": ctxCmd} {
			if got.Dir != "/child/dir/" || !slices.Equal(got.Env, want) {
				t.Errorf("%s: %s in /child/dir/ = Dir %q, Env %q; want Dir %q, Env %q", goos, name, got.Dir, got.Env, "/child/dir/", want)
			}
		}
	}
}

// Git runs git in the Env, not the process's: git reads GIT_EDITOR from it.
func TestGitRunsInTheEnv(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GIT_EDITOR", "from-process")
	out, err := Of([]string{"GIT_EDITOR=from-env"}).Git(context.Background(), t.TempDir(), nil, "var", "GIT_EDITOR").Output()
	if err != nil {
		t.Fatalf("git var GIT_EDITOR: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "from-env" {
		t.Errorf("git saw GIT_EDITOR=%q, want %q", got, "from-env")
	}
}

// Git runs in its directory with PWD naming it, as the other constructors do.
// The directory is a symlink, so the PWD its child sees tells the two apart:
// sh keeps a PWD that names the directory it runs in, here the link, and
// replaces any other with the physical path.
func TestGitGivesItsChildItsDirectoryAsPWD(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("os/exec gives no PWD here")
	}
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, link := filepath.Join(base, "repo"), filepath.Join(base, "link")
	if err := exec.Command("git", "init", "-q", repo).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	env := Of(os.Environ()).With("PWD=/", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=alias.pwd", `GIT_CONFIG_VALUE_0=!printf %s "$PWD"`)
	out, err := env.Git(context.Background(), link, nil, "pwd").Output()
	if err != nil {
		t.Fatalf("git pwd: %v", err)
	}
	if got := string(out); got != link {
		t.Errorf("the child of git run in %s saw PWD=%q, want %q", link, got, link)
	}
}
