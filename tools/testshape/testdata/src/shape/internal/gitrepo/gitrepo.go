// Package gitrepo is production code of another package that starts git.
package gitrepo

import "os/exec"

func Init(dir string) error {
	return exec.Command("git", "init", dir).Run()
}
