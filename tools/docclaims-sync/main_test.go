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

// TestWriteReportsAReanchorAndStillWrites pins the branch between the other
// two: a re-anchor is the ordinary edit, so it is reported and then written,
// never refused. The distinction is the whole reason Stopped is a separate
// kind, and nothing held it — a refusal widened to cover AnchorMoved would
// have broken the prescribed workflow with both of the other tests still
// green.
//
// It also pins the order. The report claims an action already taken, so it
// must follow the write that takes it.
func TestWriteReportsAReanchorAndStillWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest_gen.go")
	claims := []docclaims.Claim{{Doc: "d.md", Text: "some text", Src: "now inside a longer literal saying some text"}}
	reanchored := docclaims.Comparison{Drifted: []docclaims.Drift{{
		Claim: docclaims.Claim{Doc: "d.md", Text: "some text", Src: "some text"},
		Kind:  docclaims.AnchorMoved,
		Now:   "now inside a longer literal saying some text",
	}}}
	if err := write(path, claims, reanchored); err != nil {
		t.Fatalf("write() refused a re-anchor; the ordinary edit is now unprescribable: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("write() reported a re-anchor but produced no manifest: %v", err)
	}
	if !strings.Contains(string(body), `Src: "now inside a longer literal saying some text"`) {
		t.Error("write() wrote a manifest that did not carry the re-anchored source")
	}
}

// TestAReanchorReportDoesNotDumpAWholeLiteral covers a report that buried its
// own subject. A Go-literal handle is the entire literal the words were found
// inside, which reaches kilobytes in this corpus, and the writer printed it
// unedited while Explain truncated the same value for exactly that reason.
func TestAReanchorReportDoesNotDumpAWholeLiteral(t *testing.T) {
	huge := "a shipped literal that begins here " + strings.Repeat("x", 4000)
	d := docclaims.Drift{
		Claim: docclaims.Claim{Doc: "d.md", Text: "begins here", Src: "begins here"},
		Kind:  docclaims.AnchorMoved,
		Now:   huge,
	}
	brief := d.NowBrief()
	if len(brief) > 200 {
		t.Errorf("NowBrief() returned %d bytes; a report line carrying it buries the sentence it is about", len(brief))
	}
	if !strings.Contains(d.Explain(), brief) {
		t.Error("Explain() and the writer's report no longer show the same shortened handle")
	}
}
