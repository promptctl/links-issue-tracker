package workflows

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/config"
	"github.com/promptctl/links-issue-tracker/internal/pathspec"
	"github.com/promptctl/links-issue-tracker/internal/templates"
)

//go:embed defaults/*.md
var embeddedDefaultsRaw embed.FS

// embeddedDefaultsFS roots the embedded tree at "defaults" so its entries walk
// with the same root-relative paths as the project and global layers below.
var embeddedDefaultsFS = mustSub(embeddedDefaultsRaw, "defaults")

func mustSub(fsys embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		// The go:embed directive above guarantees "defaults" exists; a
		// failure here is a compile-time impossibility, not a state to
		// recover from at runtime.
		panic("workflows: embedded defaults subtree missing: " + err.Error())
	}
	return sub
}

// Load discovers every workflow definition visible to the workspace and
// resolves them into one Set: project layer first, then global, then the
// embedded defaults shipped with this binary — the same
// project > global > embedded precedence the managed templates use — merged
// by ID with the nearer layer winning.
//
// Load cannot fail: an absent layer contributes nothing, and every per-file
// problem — unreadable entry, malformed frontmatter, inert or unknown-event
// definitions — is returned as a Warning instead of an error, because a
// broken workflow file must never break a lit invocation.
func Load(workspaceRoot string) Set {
	type layer struct {
		fsys   fs.FS
		source templates.Source
	}
	layers := []layer{
		{dirFS(projectWorkflowsDir(workspaceRoot)), templates.SourceProject},
		{dirFS(globalWorkflowsDir()), templates.SourceGlobal},
		{embeddedDefaultsFS, templates.SourceEmbedded},
	}

	var (
		definitions []Definition
		warnings    []Warning
		claimed     = map[string]bool{}
	)
	for _, l := range layers {
		if l.fsys == nil {
			continue
		}
		layerDefs, layerWarns := loadLayer(l.fsys, l.source)
		warnings = append(warnings, layerWarns...)
		for _, def := range layerDefs {
			// Same ID at a farther layer is the override feature, not a
			// collision: the nearer definition already claimed it.
			if claimed[def.ID] {
				continue
			}
			claimed[def.ID] = true
			definitions = append(definitions, def)
		}
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].ID < definitions[j].ID })
	return Set{Definitions: definitions, Warnings: warnings}
}

// dirFS adapts a possibly-absent filesystem layer root to fs.FS, so Load's
// layer loop treats every layer — real directory or embedded — uniformly.
func dirFS(dir pathspec.PathSpec) fs.FS {
	if dir.IsEmpty() {
		return nil
	}
	return os.DirFS(dir.String())
}

// loadLayer walks one layer root recursively and parses every *.md file into
// a definition. The hierarchy under the root is arbitrary — only the
// ".md"-ness of a file decides participation; its location only seeds the
// default ID. Within a layer a duplicate ID keeps the first file in walk
// (lexical) order and warns about the rest.
func loadLayer(fsys fs.FS, source templates.Source) ([]Definition, []Warning) {
	var (
		definitions []Definition
		warnings    []Warning
		claimedBy   = map[string]string{}
	)
	// The callback always returns nil, so WalkDir cannot return an error.
	_ = fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An absent layer root is genuine absence, not failure.
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			warnings = append(warnings, Warning{Source: source, Path: path, Message: "cannot read: " + walkErr.Error()})
			return nil
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		raw, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			warnings = append(warnings, Warning{Source: source, Path: path, Message: "cannot read: " + readErr.Error()})
			return nil
		}
		def, parseWarns, ok := parseDefinition(string(raw), path, source)
		warnings = append(warnings, parseWarns...)
		if !ok {
			return nil
		}
		if prior, dup := claimedBy[def.ID]; dup {
			warnings = append(warnings, Warning{Source: source, Path: path, Message: fmt.Sprintf("duplicate id %q (already defined by %s): file ignored", def.ID, prior)})
			return nil
		}
		claimedBy[def.ID] = path
		definitions = append(definitions, def)
		return nil
	})
	return definitions, warnings
}

func projectWorkflowsDir(workspaceRoot string) pathspec.PathSpec {
	return pathspec.New(workspaceRoot).Join(".lit", "workflows")
}

func globalWorkflowsDir() pathspec.PathSpec {
	// [LAW:one-source-of-truth] Global workflow storage reuses config.ConfigDir
	// as the canonical root, beside the templates directory.
	return pathspec.New(config.ConfigDir()).Join("workflows")
}
