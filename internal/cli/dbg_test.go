package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/store"
)

func TestDbgEvents(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-actor")
	ctx := context.Background()
	ap := newTestCLIApp(t)
	issue, _ := ap.Store.CreateIssue(ctx, store.CreateIssueInput{Prefix: "test", Title: "x", Topic: "attribution", IssueType: "task", Priority: 0})
	var stdout bytes.Buffer
	if err := runTransition(ctx, &stdout, ap, []string{issue.ID}, "start"); err != nil { t.Fatal(err) }
	err := runTransition(ctx, &stdout, ap, []string{issue.ID}, "done")
	t.Logf("done err=%v stdout=%q", err, stdout.String())
	cur, _ := ap.Store.GetIssue(ctx, issue.ID)
	t.Logf("state=%q", cur.State())
	detail, _ := ap.Store.GetIssueDetail(ctx, issue.ID)
	t.Logf("event count=%d", len(detail.Events))
	for _, e := range detail.Events {
		t.Logf("action=%q actor=%q", e.Action, e.Actor)
	}
}
