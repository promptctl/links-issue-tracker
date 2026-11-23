package widget

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"example.com/shape/internal/cli"
	"example.com/shape/internal/engine"
	"example.com/shape/internal/gitrepo"
)

func TestOnlyComputes(t *testing.T) {
	if Sum(1, 2) != 3 {
		t.Fatal("sum")
	}
}

func TestSpawnsGit(t *testing.T) {
	if err := exec.Command("git", "version").Run(); err != nil {
		t.Fatal(err)
	}
}

func TestCallsAHelperThatRunsTheCLI(t *testing.T) {
	runInDir(t, "status")
}

func runInDir(t *testing.T, args ...string) {
	t.Helper()
	runCLI(t, args)
}

func runCLI(t *testing.T, args []string) {
	if err := cli.Run(args); err != nil {
		t.Fatal(err)
	}
}

func TestSleeps(t *testing.T) {
	time.Sleep(time.Millisecond)
}

func TestTimesOutAgainstTheClock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	<-ctx.Done()
}

func TestReadsTheClock(t *testing.T) {
	if time.Now().IsZero() {
		t.Fatal("zero")
	}
}

func TestWritesATempFile(t *testing.T) {
	if err := os.WriteFile(filepath.Join(t.TempDir(), "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOpensAStore(t *testing.T) {
	if err := engine.Open("dir"); err != nil {
		t.Fatal(err)
	}
}

// run starts the program it is handed: what it touches is decided by its
// callers' arguments.
func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if err := exec.Command(name, args...).Run(); err != nil {
		t.Fatal(err)
	}
}

func TestRunsGitThroughAHelperNamingIt(t *testing.T) {
	run(t, "git", "init")
}

func TestRunsAnotherProgram(t *testing.T) {
	run(t, "sh", "-c", "true")
}

func TestReexecsItself(t *testing.T) {
	if err := exec.Command(os.Args[0], "-test.run=^$").Run(); err != nil {
		t.Fatal(err)
	}
}

func TestReachesGitThroughProductionCode(t *testing.T) {
	if _, err := Describe("."); err != nil {
		t.Fatal(err)
	}
}

func TestReachesGitThroughAnotherPackage(t *testing.T) {
	if err := gitrepo.Init("."); err != nil {
		t.Fatal(err)
	}
}

func TestReachesGitThroughAMethod(t *testing.T) {
	if err := (Repo{dir: "."}).Init(); err != nil {
		t.Fatal(err)
	}
}

func TestCallsTheSeamThatRunsGit(t *testing.T) {
	if err := Sync(); err != nil {
		t.Fatal(err)
	}
}

func TestReplacesTheSeamThatRunsGit(t *testing.T) {
	prev := fetch
	t.Cleanup(func() { fetch = prev })
	fetch = func() error { return nil }
	if err := Sync(); err != nil {
		t.Fatal(err)
	}
}

func TestCallsCodeWithALocalNamedLikeAHelperThatRunsTheCLI(t *testing.T) {
	if Format() == "" {
		t.Fatal("empty")
	}
}

// The field git shares its name with Repo's method git, which starts git;
// reading the field calls nothing.
func TestReadsAFieldNamedLikeAMethodThatRunsGit(t *testing.T) {
	for _, c := range []struct{ git string }{{"a"}} {
		if c.git == "" {
			t.Fatal("empty")
		}
	}
}

func TestRunsGitInASubtest(t *testing.T) {
	t.Run("init", func(t *testing.T) {
		if err := exec.Command("git", "init").Run(); err != nil {
			t.Fatal(err)
		}
	})
}
