package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// brokenTitle is a title no parser admits, as a store can hold one written
// before its parser existed or carried in by a restore.
const brokenTitle = "two\nlines"

// seedStoredTitle restores the store with the issue id's title set to title,
// the one way a title no parser admits reaches the store.
func seedStoredTitle(t *testing.T, st *Store, id, title string) {
	t.Helper()
	ctx := context.Background()
	export, err := st.Export(ctx)
	if err != nil {
		t.Fatalf("Export() error = %v", err)
	}
	for i := range export.Issues {
		if export.Issues[i].ID == id {
			export.Issues[i].Title = model.Text(title)
		}
	}
	if _, err := st.ReplaceFromExport(ctx, export); err != nil {
		t.Fatalf("ReplaceFromExport() error = %v", err)
	}
	if got, err := st.GetIssue(ctx, id); err != nil || got.Title.Raw() != title {
		t.Fatalf("seeded title = %q, %v; want %q", got.Title, err, title)
	}
}

// A write that leaves a stored break title alone lands, and so does undoing
// it: neither moves the title, so nothing parses it.
func TestAWriteBesideAStoredBreakTitleLandsAndUndoes(t *testing.T) {
	ctx := context.Background()
	st, _ := newUndoStore(t)
	epic := mustCreate(t, st, storage.CreateIssueInput{Title: storage.MustParseTitle("epic"), Lane: storage.SharedLane, IssueType: "epic"})
	seedStoredTitle(t, st, epic.ID, brokenTitle)

	restated := brokenTitle
	before, _ := st.GetIssue(ctx, epic.ID)
	applied, err := st.Apply(ctx, epic.ID, storage.Change{Actor: "editor", Fields: storage.UpdateIssueInput{Title: &restated}})
	if err != nil {
		t.Fatalf("Apply(the stored title restated) error = %v", err)
	}
	if same := applied.Issue; len(applied.Fields) != 0 || !same.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("restating the stored title bumped UpdatedAt %s -> %s", before.UpdatedAt, same.UpdatedAt)
	}

	description := "now described"
	if _, err := st.Apply(ctx, epic.ID, storage.Change{Actor: "editor", Fields: storage.UpdateIssueInput{Description: &description}}); err != nil {
		t.Fatalf("Apply(description) error = %v", err)
	}
	if _, err := st.RevertWrites(ctx, []string{headWrite(t, st)}, "undoer"); err != nil {
		t.Fatalf("RevertWrites(the description edit) error = %v", err)
	}
	got, err := st.GetIssue(ctx, epic.ID)
	if err != nil {
		t.Fatalf("GetIssue() error = %v", err)
	}
	if got.Title != brokenTitle || got.Description != "" {
		t.Errorf("after the undo: title %q, description %q; want the stored title and no description", got.Title, got.Description)
	}

	moved := "other\nlines"
	_, err = st.Apply(ctx, epic.ID, storage.Change{Actor: "editor", Fields: storage.UpdateIssueInput{Title: &moved}})
	var refused model.ValidationError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "title cannot hold a line break") {
		t.Errorf("Apply(title moved to another break) error = %v, want the line-break refusal", err)
	}
}

// An unedited read-out of an issue whose stored title holds a break imports
// cleanly, whether or not the store's title moved since the read; the same
// read-out edited to a new break title is refused.
func TestAReadOutOfAStoredBreakTitleReimports(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		movedTo string
		edit    string
		want    string
		refused bool
	}{
		{name: "unedited", want: brokenTitle},
		{name: "unedited after the title moved", movedTo: "renamed", want: "renamed"},
		{name: "edited to a new break", edit: "new\nbreak", want: brokenTitle, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newUndoStore(t)
			epic := mustCreate(t, st, storage.CreateIssueInput{Title: storage.MustParseTitle("epic"), Lane: storage.SharedLane, IssueType: "epic"})
			seedStoredTitle(t, st, epic.ID, brokenTitle)
			read, err := st.GetIssue(ctx, epic.ID)
			if err != nil {
				t.Fatalf("GetIssue() error = %v", err)
			}
			doc := storage.BulkUpdateDocument(storage.IssueRelations{Issue: read})
			if tc.edit != "" {
				doc.Title = &tc.edit
			}
			if tc.movedTo != "" {
				if _, err := st.Apply(ctx, epic.ID, storage.Change{Actor: "other", Fields: storage.UpdateIssueInput{Title: &tc.movedTo}}); err != nil {
					t.Fatalf("Apply(move the title) error = %v", err)
				}
			}
			_, err = st.BulkApply(ctx, "test", "importer", []storage.BulkIssueSpec{doc})
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "title cannot hold a line break") {
					t.Errorf("BulkApply error = %v, want the line-break refusal", err)
				}
			} else if err != nil {
				t.Fatalf("BulkApply(the read-out) error = %v", err)
			}
			if got, _ := st.GetIssue(ctx, epic.ID); got.Title.Raw() != tc.want {
				t.Errorf("title = %q, want %q", got.Title, tc.want)
			}
		})
	}
}
