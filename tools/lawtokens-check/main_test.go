package main

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"
)

// marker assembles a citation at runtime so the repo gate, which scans this
// file, does not read the fixtures as citations.
func marker(namespace, token string) string {
	return "[" + namespace + ":" + token + "]"
}

func TestRunExitCodes(t *testing.T) {
	fsys := fstest.MapFS{
		"clean.go":    {Data: []byte("// " + marker("LAW", "no-silent-failure") + "\n")},
		"invented.go": {Data: []byte("// " + marker("LAW", "no-silent-fallbacks") + "\n")},
	}

	cases := []struct {
		name       string
		paths      []string
		wantCode   int
		wantStderr string
	}{
		{name: "no files", paths: nil, wantCode: 0},
		{name: "canonical markers only", paths: []string{"clean.go"}, wantCode: 0},
		{name: "an invented token", paths: []string{"clean.go", "invented.go"}, wantCode: 1, wantStderr: "invented.go:1: " + marker("LAW", "no-silent-fallbacks")},
		{name: "an unreadable file", paths: []string{"missing.go"}, wantCode: 2, wantStderr: "missing.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := run(fsys, tc.paths, &stderr); got != tc.wantCode {
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
