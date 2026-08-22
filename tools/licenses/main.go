// licenses generates the third-party-dependency compliance artifacts a
// distributed lit binary ships: THIRD_PARTY_LICENSES (the full license text of
// every module compiled into the binary), LICENSE-REPORT.md (a human-readable
// module-to-license table with a summary), and — when -sbom is given — a
// CycloneDX SBOM (a machine-readable bill of materials for vulnerability
// scanners). All three are different renderings of ONE inventory: the modules
// `go list -deps ./cmd/lit` resolves (classified once), plus the curated native
// C libraries cgo static-links but go.mod tooling can't see (see native.go).
// Deriving them from the same entries is the point — the SBOM provably describes
// the identical set the license report does, so the two release artifacts can
// never disagree about what is in the binary. [LAW:one-source-of-truth]
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
// That wider graph is not unexamined — it is what -graph below measures — but
// it is deliberately not what the bundle, report, and SBOM describe, because
// those three assert what is IN the binary and must not name a module that
// isn't.
//
// Reading pointer: a one-time hand-authored analysis of the full graph lives
// in LICENSE-ANALYSIS.md / docs/license-inventory.tsv in the repo history.
// This tool supersedes it in both scopes now — the linked set by generation,
// the full graph by -graph — so neither number is a thing a human measured
// once and wrote down.
//
// A second mode, -check, is the CI license-policy gate: it builds the same
// inventory and fails (non-zero exit) if any linked module's classified license
// is outside the committed policy (policy.json) — the allowlist of permissive
// licenses plus documented per-module exceptions. It writes no artifacts.
// Because it shares buildEntries with generation, it checks the exact licenses
// the report documents. [LAW:single-enforcer]
//
// A third mode, -graph, audits a deliberately different set: every module in
// the go.mod build list (`go list -m all`), linked or not. The two scopes
// answer two questions that have different right answers. What the binary
// links decides what lit must attribute and what legally binds it; what go.mod
// declares decides what an auditor — or a policy engine reading go.sum — will
// see, and that set is larger and carries coordinates the compiler never
// touches. Keeping -graph out of the shipped artifacts is the point: it must
// never add a module to the SBOM that is not in the binary. It shares the
// classifier and the policy with the other two modes so the word "permissive"
// cannot come to mean two things, and it reports rather than gates — see
// WriteGraphReport for why the measurement chose that. [LAW:single-enforcer]
//
// Invocation:
//
//	go run ./tools/licenses \
//	  -pkg ./cmd/lit \
//	  -bundle THIRD_PARTY_LICENSES \
//	  -report LICENSE-REPORT.md \
//	  -sbom SBOM.cdx.json \
//	  -app-version 0.2.0
//
//	go run ./tools/licenses -check -pkg ./cmd/lit   # license-policy gate
//	go run ./tools/licenses -graph                  # module-graph audit
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	lc "github.com/google/licenseclassifier"
)

// Entry is one third-party component compiled into the binary — a Go module or
// a statically-linked native C library — paired with its resolved license: the
// file it came from (Go modules only), the classifier's verdict, the raw text
// WriteBundle must ship verbatim, and the package URL the SBOM records.
// PackageURL is carried on the Entry (not recomputed in buildSBOM) so a Go
// module gets a pkg:golang purl and a native lib a pkg:generic one without the
// SBOM renderer branching on origin. [LAW:dataflow-not-control-flow]
type Entry struct {
	Module      Module
	LicenseFile string
	LicenseName string
	Confidence  float64
	Text        string
	PackageURL  string
	// Note is reader-facing context the license name alone doesn't carry — a
	// dual-license election, or the provenance behind a compound expression.
	// Curated per native lib (native.go); empty for classified Go modules. It
	// renders in LICENSE-REPORT.md's Notes section and as the SBOM component's
	// description, so the shipped artifacts explain themselves.
	Note string
	// Acknowledgement records WHOSE license claim this row is: acknowledgementConcluded
	// for one lit arrived at — an election out of a dual grant, or an expression
	// composed from the ported sources' own headers — and empty for a row that
	// merely reports what a license file says. It is the machine-readable half
	// of Note: prose in a description explains an election to a person, while a
	// policy engine that independently resolves the coordinate needs a field to
	// tell a considered choice from a contradiction. Consumed by the SBOM only.
	Acknowledgement string
}

func main() {
	var (
		pkg        = flag.String("pkg", "./cmd/lit", "package whose linked dependency set to scan, as passed to `go list -deps`")
		bundlePath = flag.String("bundle", "THIRD_PARTY_LICENSES", "output path for the third-party attribution bundle")
		reportPath = flag.String("report", "LICENSE-REPORT.md", "output path for the human-readable license report")
		sbomPath   = flag.String("sbom", "", "output path for the CycloneDX SBOM (empty: skip SBOM generation)")
		appVersion = flag.String("app-version", "", "lit version to record as the SBOM's subject component (empty: omit the version)")
		check      = flag.Bool("check", false, "license-policy gate mode: verify every linked module's license against policy.json and exit non-zero on any violation; writes no artifacts")
		graph      = flag.Bool("graph", false, "module-graph audit mode: classify every module `go list -m all` resolves — the set an auditor reading go.mod/go.sum sees, not just what the binary links — and report every license outside policy.json; writes no artifacts and does not gate")
	)
	flag.Parse()

	op, err := selectMode(*check, *graph)
	if err == nil {
		err = op.run(*pkg, *bundlePath, *reportPath, *sbomPath, *appVersion, os.Stdout)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "licenses: %v\n", err)
		os.Exit(1)
	}
}

// mode is the single operation this invocation performs. The command line
// offers the modes as independent booleans for backward compatibility — CI and
// release-validate.yml have passed `-check` since the gate was built — but two
// booleans can both be set, and a program that silently honoured one and
// ignored the other would do something the operator did not ask for. Collapsing
// them into one value at the boundary makes that state unrepresentable
// everywhere downstream. [LAW:types-are-the-program]
type mode int

const (
	modeGenerate mode = iota // default: write bundle, report, and optionally SBOM
	modeCheck                // -check: the CI license-policy gate over the link closure
	modeGraph                // -graph: the module-graph audit over the whole build list
)

// selectMode is the boundary that turns the mutually-exclusive mode flags into
// the one operation to run, failing loudly when more than one is given rather
// than picking a winner. [LAW:parse-dont-validate] its output is a value that
// could not have existed before the check, so nothing downstream re-examines
// the flags or has to decide what two modes at once would mean.
func selectMode(check, graph bool) (mode, error) {
	// [LAW:no-silent-failure] `-check -graph` almost certainly means the
	// operator wanted both audits and will otherwise believe they got them.
	if check && graph {
		return 0, fmt.Errorf("-check and -graph select different operations; run the tool once for each")
	}
	if check {
		return modeCheck, nil
	}
	if graph {
		return modeGraph, nil
	}
	return modeGenerate, nil
}

// run dispatches the selected mode. Every mode takes the same arguments and
// ignores what it does not need, so main() holds one call rather than a branch
// whose arms drift apart. [LAW:dataflow-not-control-flow]
func (m mode) run(pkg, bundlePath, reportPath, sbomPath, appVersion string, stdout io.Writer) error {
	switch m {
	case modeCheck:
		return runCheck(pkg, stdout)
	case modeGraph:
		return runGraph(stdout)
	default:
		return run(pkg, bundlePath, reportPath, sbomPath, appVersion, stdout)
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
	entries, err := buildEntries(pkg)
	if err != nil {
		return err
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
		fmt.Fprintf(stdout, "licenses: wrote %s, %s, and %s for %d components\n", bundlePath, reportPath, sbomPath, len(entries))
		return nil
	}

	fmt.Fprintf(stdout, "licenses: wrote %s and %s for %d components\n", bundlePath, reportPath, len(entries))
	return nil
}

// buildEntries resolves the linked-module inventory for pkg and classifies each
// module's license — one Entry per module. This is the SINGLE enforcer of the
// classification pipeline: run() renders the bundle/report/SBOM from what this
// returns, and the end-to-end tests assert against the same function, so there
// is no second copy of "resolve modules, find the license file, classify it"
// that could pass in a test while run() emits different entries in production.
// [LAW:single-enforcer] [LAW:one-source-of-truth]
//
// It is the effectful boundary that gathers inputs (go list, license-file
// reads); the pure renderers (WriteBundle, WriteReport, buildSBOM) consume its
// output. [LAW:effects-at-boundaries]
func buildEntries(pkg string) ([]Entry, error) {
	mods, err := LinkedModules(pkg)
	if err != nil {
		return nil, fmt.Errorf("resolve linked modules: %w", err)
	}
	// An empty result almost certainly means the package argument or module
	// resolution is broken, not that the binary genuinely has zero
	// dependencies — lit links Dolt, cobra, viper, and dozens more.
	// [LAW:no-silent-failure] refuse to build an empty-but-successful-looking
	// inventory.
	if len(mods) == 0 {
		return nil, fmt.Errorf("no linked modules found for %s; refusing to write an empty bundle/report", pkg)
	}

	classifier, err := lc.New(lc.DefaultConfidenceThreshold)
	if err != nil {
		return nil, fmt.Errorf("build license classifier: %w", err)
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
			return nil, fmt.Errorf("%s@%s: %w", m.Path, m.Version, err)
		}
		text, err := os.ReadFile(licensePath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", licensePath, err)
		}
		name, confidence := Classify(classifier, string(text))
		entries = append(entries, Entry{
			Module:      m,
			LicenseFile: licensePath,
			LicenseName: name,
			Confidence:  confidence,
			Text:        string(text),
			PackageURL:  goModulePURL(m.Path, m.Version),
		})
	}

	// Native C libraries (ICU, zstd, musl, compiler-rt) are cgo-static-linked
	// into release binaries but invisible to `go list -deps`. Append the curated
	// inventory here so every consumer — bundle, report, SBOM, AND the policy
	// gate — sees the complete set of what ships, from one function. Appending
	// (rather than a separate list threaded through each renderer) is the
	// single-enforcer choice: no consumer can forget them. [LAW:single-enforcer]
	return append(entries, nativeEntries()...), nil
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
