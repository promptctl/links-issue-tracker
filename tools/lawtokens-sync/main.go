// lawtokens-sync regenerates internal/lawtokens/canonical_gen.go from the
// upstream Token index (lawtokens.UpstreamIndexURL), or with -check reports
// whether the committed file still matches it.
//
// The Token index lives outside this repository, so the repo gate
// (TestRepoMarkersAreCanonical) can only check citations against the copy
// this tool writes. This tool is the one place that copy is compared with its
// source: `just lawtokens-sync` writes it, and the nightly workflow runs
// `-check` so an upstream change shows up as its own failure, before a correct
// citation of a new law fails a PR build. [LAW:single-enforcer]
//
// Invocation, from the repository root:
//
//	go run ./tools/lawtokens-sync          # rewrite canonical_gen.go
//	go run ./tools/lawtokens-sync -check   # exit 1 if it is out of date
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/lawtokens"
)

const generatedPath = "internal/lawtokens/canonical_gen.go"

func main() {
	check := flag.Bool("check", false, "report whether "+generatedPath+" matches the upstream index instead of rewriting it")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := run(ctx, http.DefaultClient, lawtokens.UpstreamIndexURL, generatedPath, *check, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "lawtokens-sync:", err)
		os.Exit(1)
	}
}

// run fetches the index at url and either writes the rendered file to path or,
// with check, compares it to what path already holds. Both modes fetch, parse
// and render the same way; they differ only in what they do with the bytes.
func run(ctx context.Context, client *http.Client, url, path string, check bool, out io.Writer) error {
	doc, err := fetch(ctx, client, url)
	if err != nil {
		return err
	}
	index, err := lawtokens.ParseIndex(doc)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", url, err)
	}
	want := index.Render()

	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s (run from the repository root): %w", path, err)
	}
	if bytes.Equal(current, want) {
		fmt.Fprintf(out, "%s matches the upstream token index (%d keys)\n", path, len(index.Keys()))
		return nil
	}

	summary := describe(current, index.Keys())
	if check {
		return fmt.Errorf("%s is out of date with %s: %s; run `just lawtokens-sync` and commit the result", path, url, summary)
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	fmt.Fprintf(out, "rewrote %s: %s\n", path, summary)
	return nil
}

func fetch(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building request for %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching %s: HTTP %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", url, err)
	}
	return string(body), nil
}

// describe says how the file's keys differ from upstream's, reading the keys
// back out of the file itself so the report is about the bytes on disk.
func describe(current []byte, upstream []string) string {
	have, err := generatedKeys(current)
	if err != nil {
		return "the file is not generator output (" + err.Error() + ")"
	}
	inHave := map[string]bool{}
	for _, k := range have {
		inHave[k] = true
	}
	inUpstream := map[string]bool{}
	var added, removed []string
	for _, k := range upstream {
		inUpstream[k] = true
		if !inHave[k] {
			added = append(added, k)
		}
	}
	for _, k := range have {
		if !inUpstream[k] {
			removed = append(removed, k)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return "same keys, but the file differs from the generator's output (edited by hand, or reordered upstream)"
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added upstream: "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "no longer upstream: "+strings.Join(removed, ", "))
	}
	return strings.Join(parts, "; ")
}

// generatedKeys reads the string literals of canonicalKeys out of Go source.
func generatedKeys(src []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), generatedPath, src, 0)
	if err != nil {
		return nil, err
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "canonicalKeys" || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.CompositeLit)
			if !ok {
				return nil, fmt.Errorf("canonicalKeys is not a slice literal")
			}
			keys := make([]string, 0, len(lit.Elts))
			for _, elt := range lit.Elts {
				basic, ok := elt.(*ast.BasicLit)
				if !ok {
					return nil, fmt.Errorf("canonicalKeys holds a non-literal element")
				}
				key, err := strconv.Unquote(basic.Value)
				if err != nil {
					return nil, err
				}
				keys = append(keys, key)
			}
			return keys, nil
		}
	}
	return nil, fmt.Errorf("no canonicalKeys declaration")
}
