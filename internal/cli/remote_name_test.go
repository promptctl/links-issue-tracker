package cli

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func remoteName(t *testing.T, raw string) storage.RemoteName {
	t.Helper()
	name, err := storage.ParseRemoteName(raw)
	if err != nil {
		t.Fatalf("storage.ParseRemoteName(%q): %v", raw, err)
	}
	return name
}
