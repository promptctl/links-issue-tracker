package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func TestPrintIssueDetailHistoryIncludesEntryTimestamp(t *testing.T) {
	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	previousLocal := time.Local
	time.Local = denver
	t.Cleanup(func() {
		time.Local = previousLocal
	})

	issue, err := model.HydrateStatus(model.Issue{
		ID:        "links-test.1",
		Title:     "Show history timestamps",
		IssueType: "task",
		Topic:     "history",
		CreatedAt: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC),
	}, model.StatusView{Value: model.StateOpen})
	if err != nil {
		t.Fatalf("HydrateStatus() error = %v", err)
	}

	var stdout bytes.Buffer
	err = printIssueDetail(&stdout, model.IssueDetail{
		Issue: issue,
		Events: []model.IssueEvent{{
			Action:    "start",
			Reason:    "began work",
			Actor:     "alice",
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}},
	})
	if err != nil {
		t.Fatalf("printIssueDetail() error = %v", err)
	}

	want := "- [alice @ Jan 1, 2026 8:04 PM MST] start began work"
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("history entry missing timestamp line %q in:\n%s", want, stdout.String())
	}
}
