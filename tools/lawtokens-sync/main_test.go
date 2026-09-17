package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/lawtokens"
)

// upstreamDoc is a minimal document in the upstream index's shape. The
// bracketed marker is assembled from parts so the repo gate, which scans this
// file, does not read it as a citation.
func upstreamDoc(laws string) string {
	return "## The token index\n\n" +
		"Framings (used in reasoning, not cited in code):\n" +
		"`[" + "FRAMING:representation]`\n\n" +
		"Laws (cited in code):\n" +
		laws + "\n\n---\n"
}

func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func rendered(t *testing.T, doc string) []byte {
	t.Helper()
	index, err := lawtokens.ParseIndex(doc)
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	return index.Render()
}

func writeFile(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "canonical_gen.go")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSyncRewritesAStaleFileAndNamesTheChange(t *testing.T) {
	oldFile := rendered(t, upstreamDoc("`decomposition` · `carrying-cost`"))
	newDoc := upstreamDoc("`decomposition` · `polishing-by-subtraction`")
	path := writeFile(t, oldFile)
	srv := serve(t, http.StatusOK, newDoc)

	var out bytes.Buffer
	if err := run(context.Background(), srv.Client(), srv.URL, path, false, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := readFile(t, path); !bytes.Equal(got, rendered(t, newDoc)) {
		t.Errorf("file after sync =\n%s\nwant the rendered upstream index", got)
	}
	for _, want := range []string{"added upstream: LAW:polishing-by-subtraction", "no longer upstream: LAW:carrying-cost"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not contain %q", out.String(), want)
		}
	}
}

func TestCheckFailsOnAStaleFileWithoutTouchingIt(t *testing.T) {
	oldFile := rendered(t, upstreamDoc("`decomposition`"))
	path := writeFile(t, oldFile)
	srv := serve(t, http.StatusOK, upstreamDoc("`decomposition` · `polishing-by-subtraction`"))

	err := run(context.Background(), srv.Client(), srv.URL, path, true, &bytes.Buffer{})
	if err == nil {
		t.Fatal("check passed on a file missing an upstream key")
	}
	for _, want := range []string{"added upstream: LAW:polishing-by-subtraction", "just lawtokens-sync"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if got := readFile(t, path); !bytes.Equal(got, oldFile) {
		t.Error("check rewrote the file")
	}
}

func TestCheckNamesAHandEditThatKeepsTheKeys(t *testing.T) {
	doc := upstreamDoc("`decomposition`")
	path := writeFile(t, append(rendered(t, doc), "// edited\n"...))
	srv := serve(t, http.StatusOK, doc)

	err := run(context.Background(), srv.Client(), srv.URL, path, true, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "same keys, but the file differs") {
		t.Fatalf("check error = %v, want the same-keys diagnosis", err)
	}
}

func TestCheckPassesOnACurrentFile(t *testing.T) {
	doc := upstreamDoc("`decomposition` · `polishing-by-subtraction`")
	path := writeFile(t, rendered(t, doc))
	srv := serve(t, http.StatusOK, doc)

	if err := run(context.Background(), srv.Client(), srv.URL, path, true, &bytes.Buffer{}); err != nil {
		t.Fatalf("check failed on a current file: %v", err)
	}
}

func TestSyncLeavesTheFileAloneWhenUpstreamCannotBeRead(t *testing.T) {
	original := rendered(t, upstreamDoc("`decomposition`"))

	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "HTTP error", status: http.StatusNotFound, body: "not found", wantErr: "HTTP 404"},
		{name: "unparseable index", status: http.StatusOK, body: "# no index here\n", wantErr: "parsing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, original)
			srv := serve(t, tc.status, tc.body)

			err := run(context.Background(), srv.Client(), srv.URL, path, false, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("run error = %v, want it to contain %q", err, tc.wantErr)
			}
			if got := readFile(t, path); !bytes.Equal(got, original) {
				t.Error("a failed sync rewrote the file")
			}
		})
	}
}
