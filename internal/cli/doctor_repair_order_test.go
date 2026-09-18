package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// cycleRepairer reports whatever health it is given and records how many times
// it was asked, so a test can tell a diagnosis that ran before the repairs from
// one that ran after them.
type cycleRepairer struct {
	report      storage.HealthReport
	doctorCalls int
}

func (r *cycleRepairer) Doctor(context.Context) (storage.HealthReport, error) {
	r.doctorCalls++
	return r.report, nil
}

func (r *cycleRepairer) FixIntegrity(context.Context) (storage.HealthReport, error) {
	return r.report, nil
}

func (r *cycleRepairer) FixRankInversions(context.Context) (int, error) { return 0, nil }

// A repair walks up the parent chain to classify live issues, and that walk does
// not return when the hierarchy holds a loop. So on a looped workspace the
// repairs must not run at all: running them first is what made `lit doctor
// --fix` overflow the stack before it could name the loop, leaving the operator
// whose habit is --fix with no diagnosis whatsoever.
func TestDoctorSkipsEveryRepairOnALoopedHierarchy(t *testing.T) {
	t.Parallel()
	repairer := &cycleRepairer{report: storage.HealthReport{
		ParentCycle: []string{"E", "S"},
		Errors:      []string{"parent cycle: E -> S"},
	}}
	ran := false
	fixes := []doctorFix{func(context.Context, io.Writer, storage.Repairer) error {
		ran = true
		return errors.New("a repair ran on a looped hierarchy and would have overflowed the stack")
	}}

	var progress bytes.Buffer
	report, err := diagnoseThenRepair(context.Background(), &progress, repairer, fixes)
	if err != nil {
		t.Fatalf("diagnoseThenRepair() error = %v, want the cycle reported", err)
	}
	if ran {
		t.Error("a --fix ran on a workspace whose hierarchy holds a cycle")
	}
	if repairer.doctorCalls != 1 {
		t.Errorf("Doctor called %d times, want exactly 1 — the diagnosis must come first and stand alone", repairer.doctorCalls)
	}
	if len(report.ParentCycle) == 0 {
		t.Error("the returned report does not name the cycle, so the operator gets no diagnosis")
	}
	// [LAW:no-silent-failure] The skip is stated, not inferred from a quiet run.
	if !strings.Contains(progress.String(), "skipping every --fix") {
		t.Errorf("progress = %q, want it to say the repairs were skipped and why", progress.String())
	}
}

// The anti-vacuity half: on a clean hierarchy the same call must still run every
// repair, or the case above would pass against a doctor that never repairs.
func TestDoctorStillRunsRepairsOnACleanHierarchy(t *testing.T) {
	t.Parallel()
	repairer := &cycleRepairer{report: storage.HealthReport{}}
	ran := 0
	fixes := []doctorFix{
		func(context.Context, io.Writer, storage.Repairer) error { ran++; return nil },
		func(context.Context, io.Writer, storage.Repairer) error { ran++; return nil },
	}

	if _, err := diagnoseThenRepair(context.Background(), io.Discard, repairer, fixes); err != nil {
		t.Fatalf("diagnoseThenRepair() error = %v", err)
	}
	if ran != len(fixes) {
		t.Errorf("%d of %d repairs ran on a clean hierarchy", ran, len(fixes))
	}
	// Once to decide, once to describe what the repairs left behind.
	if repairer.doctorCalls != 2 {
		t.Errorf("Doctor called %d times, want 2: one diagnosis before the repairs and one report after", repairer.doctorCalls)
	}
}
