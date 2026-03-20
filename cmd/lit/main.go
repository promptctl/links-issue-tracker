package main

import (
	"context"
	"os"

	"github.com/bmf/links-issue-tracker/internal/cli"
)

func main() {
	if err := cli.Run(context.Background(), os.Stdout, os.Stderr, os.Args[1:]); err != nil {
		os.Exit(cli.WriteCommandError(os.Stderr, os.Stdout, os.Args[1:], err))
	}
}
