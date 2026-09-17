package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// marker assembles a citation at runtime so the repo gate, which scans this
// file, does not read the fixtures as citations.
func marker(namespace, token string) string {
	return "[" + namespace + ":" + token + "]"
}

func TestRun(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"clean.go":        "// " + marker("LAW", "no-silent-failure") + "\n",
		"sub/invented.go": "// " + marker("LAW", "no-silent-fallbacks") + "\n",
	}
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	invented := "sub/invented.go:1: " + marker("LAW", "no-silent-fallbacks")

	cases := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{name: "no files", args: nil, wantCode: 0},
		{name: "canonical markers only", args: []string{"clean.go"}, wantCode: 0},
		{name: "an invented token", args: []string{"clean.go", "sub/invented.go"}, wantCode: 1, wantStderr: invented},
		{name: "a dot-slash path", args: []string{"./sub/invented.go"}, wantCode: 1, wantStderr: invented},
		{name: "an unclean path", args: []string{"sub/../clean.go"}, wantCode: 0},
		{name: "an absolute path under the root", args: []string{filepath.Join(root, "sub", "invented.go")}, wantCode: 1, wantStderr: invented},
		{name: "a path outside the root", args: []string{"../elsewhere.go"}, wantCode: 1, wantStderr: "../elsewhere.go is outside the working directory"},
		{name: "an unreadable file", args: []string{"missing.go"}, wantCode: 1, wantStderr: "missing.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := run(root, tc.args, &stderr); got != tc.wantCode {
				t.Errorf("run exit = %d, want %d (stderr: %s)", got, tc.wantCode, stderr.String())
			}
			if tc.wantStderr == "" && stderr.Len() != 0 {
				t.Errorf("run wrote to stderr on success: %s", stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}
