package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/bmf/links-issue-tracker/internal/model"
)

func TestHashExportRefusesUnhydratedIssue(t *testing.T) {
	export := model.Export{
		Version:     1,
		WorkspaceID: "ws",
		ExportedAt:  time.Now().UTC(),
		Issues:      []model.Issue{{ID: "unhydrated-x", IssueType: "task"}},
	}
	_, err := hashExport(export)
	if err == nil || !strings.Contains(err.Error(), "is not hydrated") {
		t.Fatalf("hashExport error = %v, want not hydrated error", err)
	}
}
