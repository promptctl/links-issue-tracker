package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// envelopeTagShape matches anything written as an instructions tag rather than
// only today's spelling, so an emitter still carrying an old name after the
// constants are renamed is found instead of skipped.
var envelopeTagShape = regexp.MustCompile(`</?[\w-]*instructions[\w-]*>`)

// TestEnvelopeDelimitersHaveOneSpelling holds every emitter of an agent-instruction
// envelope to the delimiter constants the quoter defuses. An emitter that spells
// the tag itself keeps printing the old tag after a rename, while remote text
// carrying that old tag is no longer defused — a reopened injection nothing else
// would notice. Go code names the constants, so a Go string literal spelling a tag
// anywhere but their declaration fails; embedded text under internal/ cannot name
// a constant, so it must spell exactly what they hold.
func TestEnvelopeDelimitersHaveOneSpelling(t *testing.T) {
	t.Parallel()
	moduleRoot := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != moduleRoot && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(path, "_test.go"):
			return nil
		case strings.HasSuffix(path, ".go"):
			return checkGoLiterals(t, fset, path)
		case strings.HasPrefix(filepath.ToSlash(path), "../../internal/"):
			return checkEmbeddedText(t, path)
		default:
			return nil
		}
	})
	if err != nil {
		t.Fatalf("walk module sources: %v", err)
	}
}

func checkGoLiterals(t *testing.T, fset *token.FileSet, path string) error {
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return err
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.ValueSpec:
			return !declaresEnvelopeDelimiter(n)
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(n.Value)
			if err != nil {
				t.Fatalf("%s: unquote %s: %v", fset.Position(n.Pos()), n.Value, err)
			}
			if envelopeTagShape.MatchString(text) {
				t.Errorf("%s spells an envelope tag in a string literal; write agentInstructionsOpen/agentInstructionsClose instead", fset.Position(n.Pos()))
			}
		}
		return true
	})
	return nil
}

func checkEmbeddedText(t *testing.T, path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, tag := range envelopeTagShape.FindAllString(string(content), -1) {
		if tag != agentInstructionsOpen && tag != agentInstructionsClose {
			t.Errorf("%s writes %q, which is neither delimiter the quoter defuses", path, tag)
		}
	}
	return nil
}

func declaresEnvelopeDelimiter(spec *ast.ValueSpec) bool {
	for _, name := range spec.Names {
		if name.Name == "agentInstructionsOpen" || name.Name == "agentInstructionsClose" {
			return true
		}
	}
	return false
}
