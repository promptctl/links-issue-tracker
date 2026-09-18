package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/docclaims"
)

// stopped is the drift that regenerating would erase: a message the
// specification still quotes that nothing in the product carries.
func stopped() docclaims.Comparison {
	return docclaims.Comparison{Drifted: []docclaims.Drift{{
		Claim: docclaims.Claim{Doc: "06-issue-commands.md", Text: "on your path", Src: "blocked on %s (unclaimed, on your path)"},
		Kind:  docclaims.Stopped,
	}}}
}

// TestWriteRefusesToEraseAStoppedMessage covers the one action that can defeat
// this gate. Every other report compares the manifest against the tree; the
// write path did not, so a contributor regenerating for a legitimate reason
// took an unrelated stopped message with them and left every check green.
func TestWriteRefusesToEraseAStoppedMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest_gen.go")
	err := write(path, []docclaims.Claim{{Doc: "d.md", Text: "some text", Src: "some text"}}, stopped())
	if err == nil {
		t.Fatal("write() regenerated over a message that stopped shipping; the specification is now false and every check is green")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("write() refused but wrote the file anyway")
	}
	if !strings.Contains(err.Error(), "refusing to write") {
		t.Errorf("write() error = %q, want it to say it refused", err)
	}
}

// TestWriteRecordsACleanDerivation is the other side: the refusal must not be
// so broad that the ordinary regeneration stops working.
func TestWriteRecordsACleanDerivation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest_gen.go")
	claims := []docclaims.Claim{{Doc: "d.md", Text: "some text", Src: "some text"}}
	if err := write(path, claims, docclaims.Comparison{}); err != nil {
		t.Fatalf("write() on a clean comparison: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading what write() produced: %v", err)
	}
	if !strings.Contains(string(body), `Text: "some text"`) {
		t.Error("write() produced a manifest without the derived entry")
	}
}

// TestTheExitLineCountsWhatDiffers covers a summary that contradicted the lines
// above it. One chapter dropping a quotation while another adds one leaves both
// totals equal, and the old line reported "committed 1102 quotations, the tree
// yields 1102" as evidence of a difference.
func TestTheExitLineCountsWhatDiffers(t *testing.T) {
	err := verify(docclaims.Comparison{
		Drifted: []docclaims.Drift{{
			Claim: docclaims.Claim{Doc: "a.md", Text: "one message", Src: "one message"},
			Kind:  docclaims.QuoteDropped,
		}},
		Added: []docclaims.Claim{{Doc: "b.md", Text: "another message", Src: "another message"}},
	})
	if err == nil {
		t.Fatal("verify() accepted a manifest that disagrees with the tree")
	}
	if strings.Contains(err.Error(), "the tree yields 0") || !strings.Contains(err.Error(), "1 recorded quotation(s)") {
		t.Errorf("verify() error = %q, want it to count what differs rather than state two totals", err)
	}
}
