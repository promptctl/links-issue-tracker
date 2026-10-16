// Package storagetest holds fixtures that build workspace shapes through the
// storage.Store contract, for tests in any package that holds a store.
package storagetest

import (
	"context"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// StoreBlocksWithinEpic stores dependent blocked-by dependency where both are
// leaves of one epic. Writing that edge is refused, so it is stored the way a
// workspace comes to hold one: written while dependent sits in another epic,
// and kept when dependent moves back. A reparent changes no rank, so every
// rank a test set stands, and every view meets the edge as it would in a
// workspace. The epic dependent waited in is purged, leaving nothing else.
func StoreBlocksWithinEpic(t testing.TB, ctx context.Context, st storage.Store, prefix, dependent, dependency string) {
	t.Helper()
	detail, err := st.GetIssueDetail(ctx, dependent)
	if err != nil {
		t.Fatalf("GetIssueDetail(%s) error = %v", dependent, err)
	}
	if detail.Parent == nil {
		t.Fatalf("%s has no epic to keep the edge inside", dependent)
	}
	holder, err := st.CreateIssue(ctx, storage.CreateIssueInput{Prefix: prefix, Title: "Holder", Topic: "holder", IssueType: "epic"})
	if err != nil {
		t.Fatalf("CreateIssue(holder) error = %v", err)
	}
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: dependent, ParentID: holder.ID, CreatedBy: "agent"}); err != nil {
		t.Fatalf("SetParent(%s into the holder) error = %v", dependent, err)
	}
	if _, err := st.AddRelation(ctx, storage.AddRelationInput{SrcID: dependent, DstID: dependency, Type: "blocked-by", CreatedBy: "agent"}); err != nil {
		t.Fatalf("AddRelation(%s blocked by %s) error = %v", dependent, dependency, err)
	}
	if _, err := st.SetParent(ctx, storage.SetParentInput{ChildID: dependent, ParentID: detail.Parent.ID, CreatedBy: "agent"}); err != nil {
		t.Fatalf("SetParent(%s back into its epic) error = %v", dependent, err)
	}
	if _, err := st.Purge(ctx, storage.PurgeInput{IssueID: holder.ID, Actor: "agent"}); err != nil {
		t.Fatalf("Purge(holder) error = %v", err)
	}
}
