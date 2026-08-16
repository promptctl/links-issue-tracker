package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// litPkg is the actual package the release binary builds, referenced by its
// full import path (not "./cmd/lit") so `go list -deps` resolves it
// regardless of the test binary's working directory.
const litPkg = "github.com/promptctl/links-issue-tracker/cmd/lit"

// TestEndToEndAgainstLitCoversDolt is this ticket's acceptance criterion
// (links-supply-chain-w6m9.1) expressed as a test: build the real inventory
// for the real release package, and confirm the bundle carries the full
// license text of a known linked dependency (github.com/dolthub/dolt) and
// the report classifies it correctly. It goes through buildEntries — the same
// pipeline run() uses — so it asserts against production's inventory, not a
// copy. [LAW:single-enforcer]
func TestEndToEndAgainstLitCoversDolt(t *testing.T) {
	entries, err := buildEntries(litPkg)
	if err != nil {
		t.Fatalf("buildEntries(%s): %v", litPkg, err)
	}

	var doltEntry *Entry
	for i := range entries {
		if entries[i].Module.Path == "github.com/dolthub/dolt/go" {
			doltEntry = &entries[i]
		}
	}

	if doltEntry == nil {
		t.Fatalf("github.com/dolthub/dolt/go not found among %d linked modules", len(entries))
	}
	if doltEntry.LicenseName != "Apache-2.0" {
		t.Errorf("dolt classified as %q, want Apache-2.0", doltEntry.LicenseName)
	}
	if !strings.Contains(doltEntry.Text, "Apache License") {
		t.Errorf("dolt's bundled text doesn't contain the Apache License header:\n%s", doltEntry.Text)
	}

	var bundle, report strings.Builder
	if err := WriteBundle(&bundle, entries); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if err := WriteReport(&report, entries); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if !strings.Contains(bundle.String(), "github.com/dolthub/dolt/go") {
		t.Error("bundle doesn't mention github.com/dolthub/dolt/go")
	}
	if !strings.Contains(report.String(), "| github.com/dolthub/dolt/go | ") {
		t.Error("report doesn't list github.com/dolthub/dolt/go")
	}
}

// TestRunHappyPath exercises main()'s orchestration directly (not just the
// helpers it calls) against the real release package, writing both output
// files and checking the stdout summary line.
func TestRunHappyPath(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "THIRD_PARTY_LICENSES")
	reportPath := filepath.Join(dir, "LICENSE-REPORT.md")
	sbomPath := filepath.Join(dir, "SBOM.cdx.json")

	var stdout strings.Builder
	if err := run(litPkg, bundlePath, reportPath, sbomPath, "9.9.9", &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, path := range []string{bundlePath, reportPath, sbomPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", path)
		}
	}
	if want := "wrote " + bundlePath + ", " + reportPath + ", and " + sbomPath; !strings.Contains(stdout.String(), want) {
		t.Errorf("want three-artifact summary %q, got: %q", want, stdout.String())
	}
}

// TestRunWithoutSBOM covers the no-`-sbom` mode (main.go's SBOM-less branch):
// an empty sbomPath writes only the bundle + report, produces no SBOM file, and
// emits the two-artifact summary line. TestRunHappyPath exercises the
// three-artifact path, so without this the return-and-log branch for this still
// -supported CLI invocation would be uncovered. [LAW:verifiable-goals]
func TestRunWithoutSBOM(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "THIRD_PARTY_LICENSES")
	reportPath := filepath.Join(dir, "LICENSE-REPORT.md")
	sbomPath := filepath.Join(dir, "SBOM.cdx.json")

	var stdout strings.Builder
	if err := run(litPkg, bundlePath, reportPath, "", "", &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, path := range []string{bundlePath, reportPath} {
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			t.Errorf("%s missing or empty: %v", path, err)
		}
	}
	if _, err := os.Stat(sbomPath); !os.IsNotExist(err) {
		t.Errorf("no SBOM should be written for an empty sbomPath, but %s exists", sbomPath)
	}
	if want := "wrote " + bundlePath + " and " + reportPath; !strings.Contains(stdout.String(), want) {
		t.Errorf("want two-artifact summary %q, got: %q", want, stdout.String())
	}
}

// TestRunEmptyModulesGuard pins the "no linked modules found" error path.
// "fmt" is a real, valid `go list -deps` target that pulls in zero external
// modules (stdlib only) — a deterministic way to hit the guard without a
// fake LinkedModules implementation.
func TestRunEmptyModulesGuard(t *testing.T) {
	dir := t.TempDir()
	err := run("fmt", filepath.Join(dir, "bundle"), filepath.Join(dir, "report"), "", "", &strings.Builder{})
	if err == nil {
		t.Fatal("want error for a package with zero linked modules")
	}
	if !strings.Contains(err.Error(), "no linked modules found") {
		t.Errorf("error = %v, want it to name the empty-modules guard", err)
	}
}

// TestRunUnwritableBundlePath pins writeFile's create-error path as surfaced
// through run(): a bundle path inside a directory that doesn't exist.
func TestRunUnwritableBundlePath(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "no-such-subdir", "THIRD_PARTY_LICENSES")
	reportPath := filepath.Join(dir, "LICENSE-REPORT.md")

	err := run(litPkg, bundlePath, reportPath, "", "", &strings.Builder{})
	if err == nil {
		t.Fatal("want error: bundle path's parent directory doesn't exist")
	}
	if !strings.Contains(err.Error(), "create") {
		t.Errorf("error = %v, want it to name the failing create", err)
	}
	if _, statErr := os.Stat(reportPath); statErr == nil {
		t.Error("report should not have been written when the bundle write failed first")
	}
}

// TestSelectModeAcceptReject is the accept/reject table for the mode flags.
// The command line exposes the modes as independent booleans, so "both set" is
// a shape a user can type; the boundary's job is to make sure it never becomes
// a shape the rest of the program has to interpret. Silently honouring one and
// ignoring the other is the failure this table forbids — an operator who asked
// for two audits and got one would have no way to notice.
// [LAW:parse-dont-validate]
func TestSelectModeAcceptReject(t *testing.T) {
	for _, tc := range []struct {
		name         string
		check, graph bool
		want         mode
		wantErr      bool
	}{
		{name: "no flags generates the artifacts", want: modeGenerate},
		{name: "-check runs the policy gate", check: true, want: modeCheck},
		{name: "-graph runs the module-graph audit", graph: true, want: modeGraph},
		{name: "-check -graph is refused, not silently resolved", check: true, graph: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectMode(tc.check, tc.graph)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("selectMode(%v, %v) = %v, want an error", tc.check, tc.graph, got)
				}
				if !strings.Contains(err.Error(), "-check") || !strings.Contains(err.Error(), "-graph") {
					t.Errorf("error must name both conflicting flags, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectMode(%v, %v): %v", tc.check, tc.graph, err)
			}
			if got != tc.want {
				t.Errorf("selectMode(%v, %v) = %v, want %v", tc.check, tc.graph, got, tc.want)
			}
		})
	}
}

// TestGraphModeWritesNoArtifacts pins that -graph is read-only. It shares its
// argument list with the generating mode, so a dispatch mistake would have it
// silently overwriting THIRD_PARTY_LICENSES with a whole-graph inventory —
// which would then ship, asserting that lit's binary contains 588 modules it
// does not link. [LAW:no-silent-failure]
func TestGraphModeWritesNoArtifacts(t *testing.T) {
	requireWholeGraph(t)

	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "THIRD_PARTY_LICENSES")
	reportPath := filepath.Join(dir, "LICENSE-REPORT.md")
	sbomPath := filepath.Join(dir, "SBOM.cdx.json")

	var stdout strings.Builder
	if err := modeGraph.run(litPkg, bundlePath, reportPath, sbomPath, "9.9.9", &stdout); err != nil {
		t.Fatalf("graph mode: %v", err)
	}

	for _, path := range []string{bundlePath, reportPath, sbomPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("graph mode wrote %s; it must write no artifacts", path)
		}
	}
	if !strings.Contains(stdout.String(), "license graph audit:") {
		t.Errorf("graph mode did not emit its report to stdout: %q", stdout.String())
	}
}

// TestLinkedModulesDeterministic pins the "deterministic output for a fixed
// dependency set" acceptance criterion: two independent runs against the same
// package must resolve to byte-identical module lists.
func TestLinkedModulesDeterministic(t *testing.T) {
	first, err := LinkedModules(litPkg)
	if err != nil {
		t.Fatalf("LinkedModules: %v", err)
	}
	second, err := LinkedModules(litPkg)
	if err != nil {
		t.Fatalf("LinkedModules: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("module count differs across runs: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("module %d differs across runs: %+v vs %+v", i, first[i], second[i])
		}
	}
}
