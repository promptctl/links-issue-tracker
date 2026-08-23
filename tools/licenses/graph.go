package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	lc "github.com/google/licenseclassifier"
)

// The graph audit answers a different question than the link-closure gate does.
// `-check` asks "what is compiled into the shipped binary" — the set an
// attribution bundle must cover. This mode asks "what does an auditor reading
// go.mod and go.sum see" — a strictly larger set, because go.sum pins every
// module in the build list whether or not the compiler ever links it. Those
// extra coordinates carry no legal obligation on lit and every automated
// scanner reports them anyway, which is the whole problem: a policy engine
// matching license strings does not know or care that a module is unreachable
// from ./cmd/lit.
//
// Two kinds of scanner read this repository and they disagree about what a
// module's license IS, so the audit models both. A coordinate scanner resolves
// `module@version` against a license database and reports one license per
// module — that is the module's own root grant. A tree-walking scanner
// (FOSSA, Black Duck, and every corporate SBOM pipeline) reads files and
// reports every license text it finds at any depth, which surfaces vendored
// test corpora, license-detection fixtures, and dual-license option files that
// impose nothing on anyone. Reporting only the first understates the audit
// surface; reporting only the second buries the real obligations under noise.
// So a hit carries the path it was found at, and root-versus-nested is derived
// from that path rather than stored beside it. [LAW:one-source-of-truth]

// graphModuleTemplate emits one moduleFields record per module in the build
// list. Unlike linkedModuleTemplate this iterates modules directly (`go list
// -m`), so dot is already a module and no `with` is needed, and there is no
// stdlib to skip; only the main module is dropped, for the same reason the
// linked scan drops it — lit's own code is not a third-party dependency it must
// account for. The record layout itself is moduleFields (modules.go), shared
// with the linked scan because parseModuleList reads both.
const graphModuleTemplate = `{{if not .Main}}` + moduleFields + `{{end}}`

// GraphModules resolves every module in the go.mod build list — `go list -m
// all`, the set an auditor reading go.mod/go.sum sees — with the source of
// each one present on disk.
//
// It runs `go mod download all` FIRST, and that ordering is the point. `go
// list -m all` reports an EMPTY .Dir for any module the cache has not fetched,
// and at the time this was written 330 of the 589 modules in this repo's graph
// had never been downloaded — so a scan that merely read what `go list`
// offered would have silently classified 56% of the graph as "no license
// found" and reported a clean bill of health for modules it never opened.
// Making the download this function's own first step turns "the cache is
// populated" from a precondition every caller must remember into a
// postcondition this function guarantees. [LAW:no-ambient-temporal-coupling]
// the ordering is owned here, not left to the caller's habits or to CI's step
// list, which is also what makes the run reproducible from a cold cache.
func GraphModules() ([]Module, error) {
	// Both go commands run inside the preservation window, and they have to.
	// The download's go.sum entries are not bookkeeping: `go list` verifies a
	// module against go.sum before it will report the module's directory, so
	// restoring the file between the two commands leaves every freshly fetched
	// module sitting in the cache with an EMPTY .Dir — present on disk and
	// invisible to the tool. Restoring after both is what keeps the audit
	// read-only without blinding it.
	var listed string
	err := withGoSumPreserved(func() error {
		// [LAW:no-silent-failure] a failed download means the graph is
		// incomplete, which would understate the audit — abort rather than
		// scan a partial set.
		download := exec.Command("go", "mod", "download", "all")
		var dlErr bytes.Buffer
		download.Stderr = &dlErr
		if err := download.Run(); err != nil {
			return fmt.Errorf("go mod download all: %w\n%s", err, dlErr.String())
		}

		cmd := exec.Command("go", "list", "-m", "-f", graphModuleTemplate, "all")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go list -m all: %w\n%s", err, stderr.String())
		}
		listed = stdout.String()
		return nil
	})
	if err != nil {
		return nil, err
	}

	mods, err := parseModuleList(listed)
	if err != nil {
		// parseModuleList rejects several distinct shapes — an empty Dir, a
		// wrong field count, a replacement whose path and version disagree — so
		// this wrap names the scope and lets the wrapped error name the cause.
		// It used to assert the empty-Dir cause outright, which was right when
		// that was the only way to get here and became misdirection the moment
		// parseReplacement added failure modes of its own: an operator told to
		// check `go mod download all` would go looking in the wrong place.
		// [LAW:no-silent-failure] the loud error must also point somewhere true.
		return nil, fmt.Errorf("resolve module graph (an empty module directory means `go mod download all` did not fetch it; other causes are named by the error itself): %w", err)
	}
	return mods, nil
}

// withGoSumPreserved runs fn and leaves go.sum byte-identical to how it found
// it, restoring the file if fn changed it and deleting it if fn created one.
//
// `go mod download all` fetches modules the build never needed, and recording
// a module it fetched means appending that module's zip hash to go.sum — 330
// new lines against this repository, none of which any build requires and all
// of which the next `go mod tidy` would strip straight back out. Leaving them
// would put the audit and tidy in a loop, each undoing the other; worse, go.sum
// is the cache key for CI's Go build cache, so an audit that rewrote it would
// invalidate a multi-gigabyte cache on every run.
//
// The deeper reason is simpler than either: an audit that modifies the file it
// audits is not an audit. Reading the whole graph is this mode's job and
// changing what the repository pins is not, so the effect the download needs
// (a populated module cache) is kept and the effect it merely causes is undone.
// [LAW:effects-at-boundaries]
func withGoSumPreserved(fn func() error) (err error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return fmt.Errorf("locate go.mod via `go env GOMOD`: %w", err)
	}
	goMod := strings.TrimSpace(string(out))
	// [LAW:no-silent-failure] outside a module `go env GOMOD` prints os.DevNull
	// or nothing at all, and there is no go.sum to protect — but there is also
	// no module graph to audit, so this is a broken invocation, not a no-op.
	if goMod == "" || goMod == os.DevNull {
		return fmt.Errorf("not inside a Go module (go env GOMOD = %q); the graph audit must run from the repository", goMod)
	}
	path := filepath.Join(filepath.Dir(goMod), "go.sum")

	before, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("read %s: %w", path, readErr)
	}
	existedBefore := readErr == nil

	// Deferred, so the restore survives every way fn can end — including a
	// panic in the go subprocesses. An undeferred restore is only a restore on
	// the paths you thought of, and the state it fails to undo (a go.sum
	// carrying hundreds of spurious lines) is one an agent may well commit
	// without noticing. Errors are JOINED rather than replaced: a failed
	// download that also trips a failed restore must not report only the
	// restore, or the real cause is lost exactly when it matters.
	// [LAW:no-silent-failure]
	defer func() {
		after, readAfterErr := os.ReadFile(path)
		if readAfterErr != nil && !os.IsNotExist(readAfterErr) {
			err = errors.Join(err, fmt.Errorf("re-read %s: %w", path, readAfterErr))
			return
		}
		switch {
		case !existedBefore && readAfterErr == nil:
			if rmErr := os.Remove(path); rmErr != nil {
				err = errors.Join(err, fmt.Errorf("remove the %s the audit created: %w", path, rmErr))
			}
		case existedBefore && (readAfterErr != nil || !bytes.Equal(before, after)):
			if wErr := os.WriteFile(path, before, 0o644); wErr != nil {
				err = errors.Join(err, fmt.Errorf("restore %s after the audit modified it: %w", path, wErr))
			}
		}
	}()

	return fn()
}

// licenseTextDirs are directory names whose entire purpose is to hold license
// texts, so any file inside one is a candidate regardless of its own name.
// This route exists because github.com/golang/freetype — the GPL coordinate
// this audit was written to find — keeps its root LICENSE as a pointer
// document ("your choice of the FreeType License or the GPL, texts in
// licenses/") and the actual GPL text in licenses/gpl.txt. A scan keyed only
// on filenames classifies that module Unknown and never sees the GPL at all.
var licenseTextDirs = map[string]bool{"licenses": true, "license": true}

// licenseNameHint matches any basename that mentions a license, deliberately
// far wider than classify.go's licenseFilePattern. That pattern answers "which
// file is this module's canonical grant", where precision matters because the
// answer feeds an attribution bundle. This one answers "which files are worth
// reading", where the classifier — not the filename — makes the actual ruling,
// so a wide net costs a few hundred cheap reads and a narrow one costs a
// missed obligation. [LAW:single-enforcer] the classifier is the one authority
// on what a license is; this regexp only bounds what gets shown to it.
var licenseNameHint = regexp.MustCompile(`(?i)licen[sc]e|copying|copyright`)

// maxLicenseFileSize caps what the scanner will read into memory to classify.
// The largest genuine license text in this repo's graph is 90 KB (Apache
// Arrow's bundled-dependency LICENSE.txt) and the largest concatenated bundle
// is 1.2 MB (the dolt fork's Godeps/LICENSES), while the largest file matching
// licenseNameHint is a 5.5 MB binary database — licenseclassifier's own corpus
// of license texts, which is data for a license detector rather than a grant
// binding anyone. 4 MiB clears every real text with room to spare and keeps a
// pathological file from being read.
//
// A file past the cap is reported AS SKIPPED rather than dropped, so the cap
// alone never hides a finding — with one deliberate exception: a file that is
// both oversize and machine content (that 5.5 MB .db) is dropped, because
// isLicenseText rules it out on its extension the same way it would if the
// classifier had read it and found nothing. The cap changes what gets read, not
// what counts as a license. [LAW:no-silent-failure]
const maxLicenseFileSize = 4 << 20

// oversizeLicense is the License value recorded for a candidate past
// maxLicenseFileSize. It is deliberately not unclassifiedLicense: "we could
// not match this text" and "we declined to read this file" are different
// facts, and collapsing them would let a skipped file read as a classified
// one. [LAW:no-silent-failure]
const oversizeLicense = "Skipped (oversize)"

// LicenseHit is one license-bearing file found inside a module, classified.
// RelPath is relative to the module root and is the field the whole audit
// turns on: a hit at the root is the module's own grant — what a coordinate
// scanner reports for module@version, and what actually binds lit — while a
// hit further down is a file some tree-walking scanner will flag, which is
// usually a vendored test corpus imposing nothing. Storing the path and
// deriving that distinction (see IsRootGrant) keeps the two from ever
// disagreeing. [LAW:one-source-of-truth]
type LicenseHit struct {
	RelPath    string
	License    string
	Confidence float64
}

// IsRootGrant reports whether the hit is the module's own license grant — a
// license file sitting directly in the module root — as opposed to one nested
// somewhere in its tree.
func (h LicenseHit) IsRootGrant() bool { return filepath.Dir(h.RelPath) == "." }

// GraphEntry is one module of the go.mod build list together with every
// license text found anywhere beneath its root. Hits is empty for a module
// that ships no license file at all, which is itself a reportable fact rather
// than a reason to omit the module — the audit's claim is that it covered the
// whole build list, and a module can only be shown to have been covered if it
// appears. [LAW:no-silent-failure]
type GraphEntry struct {
	Module Module
	Hits   []LicenseHit
}

// scanLicenseTexts returns every license-text candidate beneath root, as paths
// relative to root, in sorted order.
//
// It walks the whole module tree rather than reading the root directory,
// because the findings this audit exists to surface are precisely the ones a
// top-level read cannot see: modernc.org/libc classifies BSD-2-Clause at its
// root and carries a full GPL-2.0 COPYING seven levels down, inside a vendored
// libc conformance corpus at testdata/nsz.repo.hu/libc-test/.
// [LAW:effects-at-boundaries] this is the one filesystem-facing half of the
// graph scan; classification of what it returns is pure.
func scanLicenseTexts(root string) ([]string, error) {
	var rel []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// [LAW:no-silent-failure] an unreadable directory means part of
			// the module went unexamined; that must abort the audit rather
			// than shrink its coverage without saying so.
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if d.IsDir() {
			return nil
		}
		if !isLicenseCandidate(path) {
			return nil
		}
		r, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("relativize %s against %s: %w", path, root, relErr)
		}
		rel = append(rel, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(rel)
	return rel, nil
}

// isLicenseCandidate reports whether path is worth reading and classifying:
// either its own name mentions a license, or it sits inside a directory whose
// name says its contents are license texts. Two routes in, because the two
// conventions in the wild are genuinely different — a module either names the
// file (LICENSE, COPYING.LESSER, Sun-LICENSE) or names the folder
// (licenses/gpl.txt) — and covering only one of them was the gap that hid the
// graph's only GPL.
//
// Nothing is excluded here on the strength of its extension or its path. A
// .go file whose name happens to contain "license" reaches the classifier,
// comes back unmatched, and is filtered at report time by a rule that can see
// the classification — which is a decision made from evidence rather than from
// a hand-maintained list of extensions that would drift the moment a
// dependency added one. [LAW:dataflow-not-control-flow] the same operations
// run for every file; only the values differ.
func isLicenseCandidate(path string) bool {
	if licenseNameHint.MatchString(filepath.Base(path)) {
		return true
	}
	return licenseTextDirs[strings.ToLower(filepath.Base(filepath.Dir(path)))]
}

// machineContentExts are file extensions whose contents are consumed by a
// program rather than read by a person. A license grant is prose someone can
// be held to, so a file with one of these extensions is not one — no matter
// what its name says.
//
// This is the second half of the wide-net scan and it runs AFTER
// classification, not instead of it, which is what makes it safe to keep the
// net wide. The set was derived by scanning this repo's graph and reading what
// came back unmatched: 117 of the 161 unclassified nested candidates were
// .go/.xml/.json/.sh/.yml/.db — Oracle's SDK models a license-management API
// in ~90 Go files named license_*.go, CycloneDX ships license fixtures as XML
// and JSON, zap has a checklicense.sh. None is a grant; all of them would
// otherwise be permanent noise in an audit read by humans.
//
// It is a rejection list rather than an allowlist of "real" extensions on
// purpose. An unforeseen extension on this list's side of the line costs one
// noise row that a reader dismisses; an unforeseen extension excluded by an
// allowlist costs a missed obligation that nobody ever sees. For an audit the
// error must point at over-reporting. [LAW:no-silent-failure]
var machineContentExts = map[string]bool{
	".go": true, ".sh": true, ".yml": true, ".yaml": true,
	".json": true, ".xml": true, ".db": true,
}

// isLicenseText reports whether a classified candidate should be recorded as a
// license text. Anything the classifier recognized is one, whatever it is
// called — that is how licenses/gpl.txt, whose name announces nothing, is
// kept. A file the classifier could not read that way — unmatched, or too
// large to have been read at all — is kept only if it could plausibly be a
// grant a human would read, because an unmatched license file is the single
// most important row in this audit (nobody can say what it permits) and must
// never be discarded merely for being unmatched.
func isLicenseText(relPath, license string) bool {
	if !licenseSentinels[license] {
		return true
	}
	return !machineContentExts[strings.ToLower(filepath.Ext(relPath))]
}

// classifyLicenseFile resolves one candidate to a license name, reporting
// oversizeLicense for a file past maxLicenseFileSize instead of reading it.
// Returning the size verdict as a license VALUE rather than signalling it to
// the caller keeps one uniform path: every candidate is classified, filtered,
// and recorded by the same three steps, and "too big to read" differs from
// "matched Apache-2.0" only in the value that flows.
// [LAW:dataflow-not-control-flow]
func classifyLicenseFile(classifier *lc.License, path string) (string, float64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() > maxLicenseFileSize {
		return oversizeLicense, 0, nil
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", path, err)
	}
	name, confidence := Classify(classifier, string(text))
	return name, confidence, nil
}

// classifyModuleLicenses reads and classifies every license-text candidate in
// one module, reusing the same Classify the bundle, report, SBOM, and policy
// gate use so a license cannot be named one thing by the graph audit and
// another by the artifacts that ship. [LAW:single-enforcer]
func classifyModuleLicenses(classifier *lc.License, m Module) ([]LicenseHit, error) {
	paths, err := scanLicenseTexts(m.Dir)
	if err != nil {
		return nil, fmt.Errorf("%s@%s: %w", m.Path, m.Version, err)
	}

	hits := make([]LicenseHit, 0, len(paths))
	for _, rel := range paths {
		license, confidence, err := classifyLicenseFile(classifier, filepath.Join(m.Dir, rel))
		if err != nil {
			return nil, err
		}
		if !isLicenseText(rel, license) {
			continue
		}
		hits = append(hits, LicenseHit{RelPath: rel, License: license, Confidence: confidence})
	}
	return hits, nil
}

// buildGraphEntries is the graph audit's inventory step, the counterpart to
// buildEntries: resolve the full build list and classify every license text in
// every module. It deliberately does NOT call buildEntries — that function
// resolves the link closure and appends the statically-linked native C
// libraries, neither of which belongs in an audit of what go.mod declares —
// but it shares everything that decides a verdict: the same Module parsing,
// the same Classify, and (through its callers) the same policy. Sharing the
// classifier rather than the whole pipeline is what keeps buildEntries the
// single enforcer of what ships while this stays the single enforcer of what
// the repository declares. [LAW:single-enforcer]
func buildGraphEntries() ([]GraphEntry, error) {
	mods, err := GraphModules()
	if err != nil {
		return nil, err
	}
	// [LAW:no-silent-failure] lit's graph is hundreds of modules; an empty
	// result is a broken resolution, not a dependency-free repository.
	if len(mods) == 0 {
		return nil, fmt.Errorf("no modules found in the go.mod graph; refusing to report an empty audit")
	}

	classifier, err := lc.New(lc.DefaultConfidenceThreshold)
	if err != nil {
		return nil, fmt.Errorf("build license classifier: %w", err)
	}

	entries := make([]GraphEntry, 0, len(mods))
	for _, m := range mods {
		hits, err := classifyModuleLicenses(classifier, m)
		if err != nil {
			return nil, err
		}
		entries = append(entries, GraphEntry{Module: m, Hits: hits})
	}
	return entries, nil
}
