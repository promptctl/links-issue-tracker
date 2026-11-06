package storage_test

import (
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/merge"
	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

func TestParseTitle(t *testing.T) {
	for _, tc := range []struct {
		raw, want, err string
	}{
		{raw: "", err: "title is required"},
		{raw: " \t ", err: "title is required"},
		{raw: "two\nlines", err: "title cannot hold a line break"},
		{raw: "two\r\nlines", err: "title cannot hold a line break"},
		{raw: "trailing newline\n", want: "trailing newline"},
		{raw: "  a title  ", want: "a title"},
	} {
		title, err := storage.ParseTitle(tc.raw)
		if tc.err != "" {
			if _, isValidation := err.(model.ValidationError); !isValidation || err.Error() != tc.err {
				t.Errorf("ParseTitle(%q) error = %v, want ValidationError %q", tc.raw, err, tc.err)
			}
			continue
		}
		if got, err := title.Text(); err != nil || got != tc.want {
			t.Errorf("ParseTitle(%q).Text() = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	if _, err := (storage.Title{}).Text(); err == nil || err.Error() != "title is required" {
		t.Errorf("Title{}.Text() error = %v, want title is required", err)
	}
}

func TestParseLane(t *testing.T) {
	for _, tc := range []struct {
		raw, want, err string
	}{
		{raw: "", want: ""},
		{raw: "   ", want: ""},
		{raw: "two\nlanes", err: "lane cannot hold a line break"},
		{raw: "two\r\nlanes", err: "lane cannot hold a line break"},
		{raw: "east\n", want: "east"},
		{raw: " east ", want: "east"},
	} {
		lane, err := storage.ParseLane(tc.raw)
		if tc.err != "" {
			if _, isValidation := err.(model.ValidationError); !isValidation || err.Error() != tc.err {
				t.Errorf("ParseLane(%q) error = %v, want ValidationError %q", tc.raw, err, tc.err)
			}
			continue
		}
		if got, err := lane.Text(); err != nil || got != tc.want {
			t.Errorf("ParseLane(%q).Text() = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	if _, err := (storage.Lane{}).Text(); err == nil {
		t.Error("Lane{}.Text() error = nil, want a refusal: the zero lane is not the parsed shared lane")
	}
}

// A title or lane is admitted only when the patch moves it: over a stored
// title no parser admits, restating it and writing another field land, and
// moving it onto a new break of any kind is refused.
func TestApplyIssueFieldsAdmitsOnlyAMovingTitleOrLane(t *testing.T) {
	stored := model.Issue{ID: "lit-x-1", Title: "two\nlines", Lane: "a\rlane", IssueType: model.TypeTask}
	restated, lane, description := stored.Title.Raw(), stored.Lane.Raw(), "described"
	if _, changes, err := storage.ApplyIssueFields(stored, storage.UpdateIssueInput{Title: &restated, Lane: &lane, Description: &description}); err != nil || len(changes) != 1 {
		t.Errorf("ApplyIssueFields(restated, description) = %v, %v; want the one description change", changes, err)
	}
	for _, patch := range []storage.UpdateIssueInput{
		{Title: textOf("new\nbreak")},
		{Title: textOf("  ")},
		{Title: textOf("line\u2028separated")},
		{Lane: textOf("new\nlane")},
	} {
		_, _, err := storage.ApplyIssueFields(stored, patch)
		if _, isValidation := err.(model.ValidationError); !isValidation {
			t.Errorf("ApplyIssueFields(%+v) error = %v, want a ValidationError", patch, err)
		}
	}
}

func textOf(s string) *string { return &s }

// A reconcile's prose resolution is the agent's edit: one that keeps the
// provisional text moves nothing and lands, even over a break title the merge
// carried in, and one onto new text holding a break is refused.
func TestAProseResolutionIsAdmittedOnlyWhenItMovesTheTitle(t *testing.T) {
	provisional := model.Issue{ID: "i1", Title: "remote\ntitle"}
	if _, changes, err := storage.ApplyProseFields(provisional, merge.ProseTexts{merge.ProseTitle: provisional.Title.Raw()}); err != nil || len(changes) != 0 {
		t.Errorf("ApplyProseFields(the provisional text) = %v, %v; want no change", changes, err)
	}
	_, _, err := storage.ApplyProseFields(provisional, merge.ProseTexts{merge.ProseTitle: "merged\ntitle"})
	if _, isValidation := err.(model.ValidationError); !isValidation {
		t.Errorf("ApplyProseFields(new text with a break) error = %v, want a ValidationError", err)
	}
}

// The read-out merge projects the file's values and never admits them, so an
// unedited read-out of a stored break title merges and lands as no change,
// even after the store's title moved; the same read-out edited onto a new
// break merges and is refused by the write.
func TestAReadOutOfAStoredBreakTitleMergesAndIsAdmittedByItsWrite(t *testing.T) {
	read := model.Issue{ID: "lit-x-1", Title: "two\nlines", IssueType: model.TypeTask}
	moved := read
	moved.Title = "renamed"
	for _, tc := range []struct {
		name    string
		now     model.Issue
		edit    *string
		refused bool
	}{
		{name: "unedited", now: read},
		{name: "unedited after the title moved", now: moved},
		{name: "edited onto a new break", now: read, edit: textOf("new\nbreak"), refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := storage.BulkUpdateDocument(storage.IssueRelations{Issue: read})
			if tc.edit != nil {
				doc.Title = tc.edit
			}
			merged, err := storage.MergeReadOut([]storage.BulkIssueSpec{doc}, standing(tc.now, nil))
			if err != nil {
				t.Fatalf("MergeReadOut error = %v", err)
			}
			file, err := storage.ParseBulkFile(merged, map[model.ID]model.IssueType{read.ID: model.TypeTask})
			if err != nil {
				t.Fatalf("ParseBulkFile error = %v", err)
			}
			if len(file.Updates) != 1 {
				t.Fatalf("updates = %d, want the document's one", len(file.Updates))
			}
			_, changes, err := storage.ApplyIssueFields(tc.now, file.Updates[0].Patch)
			if tc.refused {
				if _, isValidation := err.(model.ValidationError); !isValidation {
					t.Errorf("ApplyIssueFields error = %v, want a ValidationError", err)
				}
				return
			}
			if err != nil || len(changes) != 0 {
				t.Errorf("ApplyIssueFields = %v, %v; want no change", changes, err)
			}
		})
	}
}
