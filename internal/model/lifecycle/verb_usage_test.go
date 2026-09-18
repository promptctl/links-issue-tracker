package lifecycle

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// An ActionName is a string type, so `%s` interpolates it silently and renders
// the PERSISTED EVENT ENCODING — which is the wrong name in any sentence a
// caller reads. Nothing in the compiler objects, and the two names agree for
// seven of the eight actions, so a wrong site reads correctly almost always and
// is found by a reader holding a command that does not exist.
//
// That is not a hypothetical: writing links-cli-errors-nvmd I swept for this by
// hand twice and missed a site each time, because both live sites stash the name
// in a struct field and read it back under another spelling. A grep for the
// shape at the use site cannot see them. So the rule gets a gate instead of a
// habit. [LAW:single-enforcer]
//
// The gate has two halves, and the second is the one that keeps it honest: the
// list of ActionName-typed declarations below is asserted to be COMPLETE, so a
// ninth field added anywhere fails here rather than quietly escaping a check
// that only knows about the fields someone remembered to list.
var actionNameBearers = []string{"w.action", "e.Action"}

var (
	actionNameDecl = regexp.MustCompile(`(?m)^\s*\w+\s+(?:model\.)?ActionName\s*$`)
	fmtCall        = regexp.MustCompile(`fmt\.(?:Errorf|Sprintf)\([^\n]*`)
)

func repoRootForVerbTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory; cannot locate the repo")
		}
		dir = parent
	}
}

func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v", root, err)
	}
	files := []string{}
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasSuffix(f, ".go") {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatal("git ls-files returned no Go files — this test would pass over an empty set")
	}
	return files
}

func TestNoActionNameReachesAMessageAsItsPersistedEncoding(t *testing.T) {
	root := repoRootForVerbTest(t)
	bad := []string{}
	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading tracked file %s: %v", rel, err)
		}
		for n, line := range strings.Split(string(body), "\n") {
			for _, call := range fmtCall.FindAllString(line, -1) {
				for _, bearer := range actionNameBearers {
					if !strings.Contains(call, bearer) {
						continue
					}
					if strings.Contains(call, bearer+".Verb()") {
						continue
					}
					bad = append(bad, rel+":"+itoa(n+1)+": "+strings.TrimSpace(call))
				}
			}
		}
	}
	if len(bad) > 0 {
		t.Fatalf("an action's persisted event encoding reaches a message a caller reads; use .Verb() there:\n  %s", strings.Join(bad, "\n  "))
	}
}

// TestTheBearerListIsComplete is the half that keeps the gate above honest. A
// list of names to check is a second copy of "where ActionNames live", and a
// copy that can fall behind will: a ninth field added tomorrow would simply not
// be looked at, and the gate would keep reporting success.
func TestTheBearerListIsComplete(t *testing.T) {
	root := repoRootForVerbTest(t)
	decls := 0
	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("reading tracked file %s: %v", rel, err)
		}
		decls += len(actionNameDecl.FindAllString(string(body), -1))
	}
	// ContainerActionError.Action, and the status and retention writers in the
	// Dolt store. Two spellings cover all three: the two writers are both `w`.
	const want = 3
	if decls != want {
		t.Fatalf("found %d struct fields typed ActionName, expected %d -- a new one must be added to actionNameBearers or this gate does not look at it", decls, want)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
