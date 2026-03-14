package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bmf/links-issue-tracker/internal/cli"
)

func main() {
	if msg := cli.DeprecationWarning(filepath.Base(os.Args[0])); msg != "" {
		fmt.Fprint(os.Stderr, msg)
	}
	if err := cli.Run(context.Background(), os.Stdout, os.Stderr, os.Args[1:]); err != nil {
		os.Exit(cli.WriteCommandError(os.Stderr, os.Stdout, os.Args[1:], err))
	}
}
