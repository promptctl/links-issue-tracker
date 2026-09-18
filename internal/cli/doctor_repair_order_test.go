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
	climbed := false
	cleaned := false
	fixes := []doctorFix{
		{name: "rank", climbsHierarchy: true, run: func(context.Context, io.Writer, storage.Repairer) error {
			climbed = true
			return errors.New("a climbing repair ran on a looped hierarchy and would have overflowed the stack")
		}},
		// A repair that never reads the hierarchy cannot crash on a loop, so
		// withholding it would strand faults the operator can still fix.
		{name: "integrity", run: func(context.Context, io.Writer, storage.Repairer) error {
			cleaned = true
			return nil
		}},
	}

	var progress bytes.Buffer
	report, err := diagnoseThenRepair(context.Background(), &progress, repairer, fixes)
	if err != nil {
		t.Fatalf("diagnoseThenRepair() error = %v, want the cycle reported", err)
	}
	if climbed {
		t.Error("a repair that climbs the hierarchy ran on a workspace whose hierarchy holds a cycle")
	}
	if !cleaned {
		t.Error("a repair that never reads the hierarchy was withheld; a loop only blocks the repairs it would crash")
	}
	if len(report.ParentCycle) == 0 {
		t.Error("the returned report does not name the cycle, so the operator gets no diagnosis")
	}
	// [LAW:no-silent-failure] The skip is stated, not inferred from a quiet run.
	if !strings.Contains(progress.String(), "skipping --fix rank") {
		t.Errorf("progress = %q, want it to name the repair withheld and why", progress.String())
	}
}

// The anti-vacuity half: on a clean hierarchy the same call must still run every
// repair, or the case above would pass against a doctor that never repairs.
func TestDoctorStillRunsRepairsOnACleanHierarchy(t *testing.T) {
	t.Parallel()
	repairer := &cycleRepairer{report: storage.HealthReport{}}
	ran := 0
	fixes := []doctorFix{
		{name: "rank", climbsHierarchy: true, run: func(context.Context, io.Writer, storage.Repairer) error { ran++; return nil }},
		{name: "integrity", run: func(context.Context, io.Writer, storage.Repairer) error { ran++; return nil }},
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

// The status line is parsed by scripts, so a check that never ran must not
// render as a check that found nothing. `rank_inversions=0` is a clean result;
// a workspace whose loop stopped that check has no result to report, and no
// exit code can correct a number the reader has already been handed.
func TestDoctorStatusLineSaysUncheckedRatherThanZero(t *testing.T) {
	t.Parallel()
	looped := storage.HealthReport{
		ParentCycle: []string{"E", "S"},
		Unchecked:   []string{storage.CheckRankInversions, storage.CheckDependencyCycle},
	}
	if got := doctorFieldValue(looped, storage.CheckRankInversions, "0"); got != "unchecked" {
		t.Errorf("rank_inversions rendered %q for a check that never ran, want \"unchecked\"", got)
	}
	if got := doctorFieldValue(looped, storage.CheckDependencyCycle, "none"); got != "unchecked" {
		t.Errorf("dependency_cycle rendered %q for a check that never ran, want \"unchecked\"", got)
	}
	// Anti-vacuity: a report that ran its checks still renders their real values,
	// or this would pass against a renderer that always says "unchecked".
	clean := storage.HealthReport{}
	if got := doctorFieldValue(clean, storage.CheckRankInversions, "0"); got != "0" {
		t.Errorf("rank_inversions rendered %q on a report that ran the check, want \"0\"", got)
	}
	if got := doctorFieldValue(clean, storage.CheckDependencyCycle, "none"); got != "none" {
		t.Errorf("dependency_cycle rendered %q on a report that ran the check, want \"none\"", got)
	}
}
