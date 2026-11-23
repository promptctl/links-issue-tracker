// Package widget is the package under test. Its production code reaches git
// in several ways a test can reach in turn.
package widget

import "os/exec"

func Sum(xs ...int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total
}

// Describe starts git from production code.
func Describe(dir string) ([]byte, error) {
	return exec.Command("git", "-C", dir, "describe").Output()
}

// fetch is a seam: a test may replace it with a stand-in.
var fetch = fetchFromRemote

func fetchFromRemote() error {
	return exec.Command("git", "fetch").Run()
}

// Sync calls through the seam.
func Sync() error { return fetch() }

// Format declares a local that shares its name with a test helper that runs
// the CLI; calling the local runs nothing.
func Format() string {
	runCLI := func() string { return "formatted" }
	return runCLI()
}

type Repo struct{ dir string }

// Init is a method that starts git.
func (r Repo) Init() error { return r.git("init") }

func (r Repo) git(args ...string) error {
	return exec.Command("git", append([]string{"-C", r.dir}, args...)...).Run()
}
