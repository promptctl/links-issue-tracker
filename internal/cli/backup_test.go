package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model"
)

func TestHashExportRefusesUnhydratedIssue(t *testing.T) {
	// hashExport relies on Issue.MarshalJSON to reject unhydrated values; the
	// hydrator's post-condition keeps this path unreachable from store output,
	// but the JSON boundary still enforces if any other producer slips one in.
	export := model.Export{
		Version:     1,
		WorkspaceID: "ws",
		ExportedAt:  time.Now().UTC(),
		Issues:      []model.Issue{{ID: "unhydrated-x", IssueType: "task"}},
	}
	_, err := hashExport(export)
	if err == nil || !strings.Contains(err.Error(), "no hydrated lifecycle") {
		t.Fatalf("hashExport error = %v, want no hydrated lifecycle error", err)
	}
}
