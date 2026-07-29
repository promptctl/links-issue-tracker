// licenses generates the two artifacts a distributed lit binary needs for
// license-attribution compliance: THIRD_PARTY_LICENSES (the full license text
// of every module compiled into the binary) and LICENSE-REPORT.md (a
// human-readable module-to-license table with a summary). It is invoked by
// .github/workflows/release.yml and .github/workflows/release-validate.yml
// BEFORE goreleaser runs, so the generated files exist at the repo root for
// .goreleaser.yml's archives.files to pick up — the same pattern mkmanifest
// uses for the release manifest, generated outside goreleaser because
// goreleaser v2 has no hook point that fits.
//
// The set of modules covered is exactly what `go list -deps ./cmd/lit`
// resolves to — the modules the compiler actually links into the shipped
// binary — not the full go.mod graph (which includes modules only reachable
// via other build tags, other packages in this repo, or test-only paths).
// Reading pointer: a one-time hand-authored analysis of the full graph lives
// in LICENSE-ANALYSIS.md / docs/license-inventory.tsv in the repo history;
// this tool supersedes it for the linked set with a re-runnable generator
// rather than a static document that drifts from the dependency tree.
//
// Invocation:
//
//	go run ./tools/licenses \
//	  -pkg ./cmd/lit \
//	  -bundle THIRD_PARTY_LICENSES \
//	  -report LICENSE-REPORT.md
package main

import (
	"flag"
	"fmt"
	"os"

	lc "github.com/google/licenseclassifier"
)

// Entry pairs one linked module with its resolved license: the file it came
// from, the classifier's verdict, and the raw text WriteBundle must ship
// verbatim.
type Entry struct {
	Module      Module
	LicenseFile string
	LicenseName string
	Confidence  float64
	Text        string
}

func main() {
	var (
		pkg        = flag.String("pkg", "./cmd/lit", "package whose linked dependency set to scan, as passed to `go list -deps`")
		bundlePath = flag.String("bundle", "THIRD_PARTY_LICENSES", "output path for the third-party attribution bundle")
		reportPath = flag.String("report", "LICENSE-REPORT.md", "output path for the human-readable license report")
	)
	flag.Parse()

	mods, err := LinkedModules(*pkg)
	if err != nil {
		die("resolve linked modules: %v", err)
	}
	// An empty result almost certainly means the package argument or module
	// resolution is broken, not that the binary genuinely has zero
	// dependencies — lit links Dolt, cobra, viper, and dozens more.
	// [LAW:no-silent-failure] refuse to write empty-but-successful-looking
	// artifacts.
	if len(mods) == 0 {
		die("no linked modules found for %s; refusing to write an empty bundle/report", *pkg)
	}

	classifier, err := lc.New(lc.DefaultConfidenceThreshold)
	if err != nil {
		die("build license classifier: %v", err)
	}

	entries := make([]Entry, 0, len(mods))
	for _, m := range mods {
		licensePath, err := FindLicenseFile(m.Dir)
		if err != nil {
			// [LAW:no-silent-failure] a linked module with no discoverable
			// license file is an attribution gap, not a warning: fail the
			// build so a human resolves it (vendor an override, replace the
			// dependency) rather than shipping an incomplete bundle.
			die("%s@%s: %v", m.Path, m.Version, err)
		}
		text, err := os.ReadFile(licensePath)
		if err != nil {
			die("read %s: %v", licensePath, err)
		}
		name, confidence := Classify(classifier, string(text))
		entries = append(entries, Entry{
			Module:      m,
			LicenseFile: licensePath,
			LicenseName: name,
			Confidence:  confidence,
			Text:        string(text),
		})
	}

	if err := writeFile(*bundlePath, func(f *os.File) error { return WriteBundle(f, entries) }); err != nil {
		die("%v", err)
	}
	if err := writeFile(*reportPath, func(f *os.File) error { return WriteReport(f, entries) }); err != nil {
		die("%v", err)
	}

	fmt.Printf("licenses: wrote %s and %s for %d linked modules\n", *bundlePath, *reportPath, len(entries))
}

// writeFile creates path, runs write against it, and closes it — checking the
// Close error on the success path. A deferred Close would swallow a delayed
// write/fsync failure while the tool exits 0, leaving a truncated attribution
// bundle silently reported as generated. [LAW:no-silent-failure]
func writeFile(path string, write func(*os.File) error) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := write(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "licenses: "+format+"\n", args...)
	os.Exit(1)
}
