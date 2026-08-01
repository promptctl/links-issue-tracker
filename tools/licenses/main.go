// licenses generates the third-party-dependency compliance artifacts a
// distributed lit binary ships: THIRD_PARTY_LICENSES (the full license text of
// every module compiled into the binary), LICENSE-REPORT.md (a human-readable
// module-to-license table with a summary), and — when -sbom is given — a
// CycloneDX SBOM (a machine-readable bill of materials for vulnerability
// scanners). All three are different renderings of ONE inventory: the modules
// `go list -deps ./cmd/lit` resolves, classified once. Deriving them from the
// same entries is the point — the SBOM provably describes the identical module
// set the license report does, so the two release artifacts can never disagree
// about what is in the binary. [LAW:one-source-of-truth]
//
// It is invoked by .github/workflows/release-validate.yml BEFORE goreleaser
// runs, so the generated files exist at the repo root for .goreleaser.yml's
// archives.files to pick up (bundle + report) or for a later workflow step to
// stage as a standalone release asset (the SBOM) — the same pattern mkmanifest
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
//	  -report LICENSE-REPORT.md \
//	  -sbom SBOM.cdx.json \
//	  -app-version 0.2.0
package main

import (
	"flag"
	"fmt"
	"io"
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
		sbomPath   = flag.String("sbom", "", "output path for the CycloneDX SBOM (empty: skip SBOM generation)")
		appVersion = flag.String("app-version", "", "lit version to record as the SBOM's subject component (empty: omit the version)")
	)
	flag.Parse()

	if err := run(*pkg, *bundlePath, *reportPath, *sbomPath, *appVersion, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "licenses: %v\n", err)
		os.Exit(1)
	}
}

// run performs the full generate-and-write pipeline: resolve linked modules,
// classify each one's license, and write the bundle + report. Factored out of
// main so the orchestration — including its error paths (no linked modules,
// classifier construction failing, an unwritable output path) — is directly
// testable without spawning a subprocess or parsing flags in the test.
// [LAW:effects-at-boundaries] main() itself is left holding only flag parsing
// and the process-exit boundary.
func run(pkg, bundlePath, reportPath, sbomPath, appVersion string, stdout io.Writer) error {
	mods, err := LinkedModules(pkg)
	if err != nil {
		return fmt.Errorf("resolve linked modules: %w", err)
	}
	// An empty result almost certainly means the package argument or module
	// resolution is broken, not that the binary genuinely has zero
	// dependencies — lit links Dolt, cobra, viper, and dozens more.
	// [LAW:no-silent-failure] refuse to write empty-but-successful-looking
	// artifacts.
	if len(mods) == 0 {
		return fmt.Errorf("no linked modules found for %s; refusing to write an empty bundle/report", pkg)
	}

	classifier, err := lc.New(lc.DefaultConfidenceThreshold)
	if err != nil {
		return fmt.Errorf("build license classifier: %w", err)
	}

	entries := make([]Entry, 0, len(mods))
	for _, m := range mods {
		licensePath, err := FindLicenseFile(m.Dir)
		if err != nil {
			// [LAW:no-silent-failure] a linked module with no discoverable or
			// unambiguous license file is an attribution gap, not a warning:
			// fail the build so a human resolves it (vendor an override,
			// replace the dependency) rather than shipping an incomplete or
			// silently-guessed bundle.
			return fmt.Errorf("%s@%s: %w", m.Path, m.Version, err)
		}
		text, err := os.ReadFile(licensePath)
		if err != nil {
			return fmt.Errorf("read %s: %w", licensePath, err)
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

	if err := writeFile(bundlePath, func(f *os.File) error { return WriteBundle(f, entries) }); err != nil {
		return err
	}
	if err := writeFile(reportPath, func(f *os.File) error { return WriteReport(f, entries) }); err != nil {
		return err
	}

	// The SBOM is opt-in (empty path = skip) because it ships as a standalone
	// release asset rather than inside every archive: only the release
	// pipeline needs it, whereas the bundle + report always generate. When
	// requested it renders from the very same entries — no second module
	// resolution. [LAW:one-source-of-truth]
	if sbomPath != "" {
		if err := writeFile(sbomPath, func(f *os.File) error { return WriteSBOM(f, entries, appVersion) }); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "licenses: wrote %s, %s, and %s for %d linked modules\n", bundlePath, reportPath, sbomPath, len(entries))
		return nil
	}

	fmt.Fprintf(stdout, "licenses: wrote %s and %s for %d linked modules\n", bundlePath, reportPath, len(entries))
	return nil
}

// writeFile creates path, runs write against it, syncs it to durable storage,
// and closes it — checking every step's error on the success path. A
// deferred Close alone would swallow a failing Close; and Close (close(2))
// does not by itself guarantee previously buffered writes reached disk — only
// fsync(2) does, which is why Sync runs explicitly before Close rather than
// being folded into a "Close catches it" claim. This file IS the compliance
// artifact the tool exists to produce correctly, so a truncated or corrupted
// write must be a loud error, not a silently short-written attribution
// bundle. [LAW:no-silent-failure]
func writeFile(path string, write func(*os.File) error) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := write(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}
