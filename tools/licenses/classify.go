package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	lc "github.com/google/licenseclassifier"
)

// unclassifiedLicense marks a license file whose text didn't match any known
// SPDX license above the classifier's confidence threshold. The bundle still
// carries the file's full text either way — attribution obligations don't
// wait on our classifier's confidence. [LAW:no-silent-failure] "Unknown" is a
// visible, reported fact; a module is never silently dropped because its
// license text couldn't be matched.
const unclassifiedLicense = "Unknown"

// licenseFilePattern matches the file names Go modules conventionally use for
// their license grant: LICENSE/LICENCE, COPYING, UNLICENSE, with or without a
// suffix separated by `.`, `-`, or `_` (LICENSE.txt, LICENSE-APACHE,
// LICENSE_MIT, ...) — all three separators are real conventions in the wild,
// most visibly dual-license repos that ship LICENSE-APACHE and LICENSE-MIT
// side by side. None of the 167 modules currently linked into ./cmd/lit need
// the `-`/`_` forms, but a linked module with only a hyphenated variant would
// otherwise hit FindLicenseFile's "no license file found" error, which is a
// hard build-abort (die in main.go) rather than a soft warning — worth
// accepting the wider real-world shape now. It deliberately excludes
// README/NOTICE — those carry attribution or usage notes, not the license
// grant itself, and don't belong in a bundle whose contents are "the text
// required to accompany the binary for compliance."
var licenseFilePattern = regexp.MustCompile(`(?i)^(LICEN[SC]E|COPYING|UNLICENSE)([._-][a-zA-Z0-9]+)*$`)

// FindLicenseFile returns the module-root license file for dir, per
// licenseFilePattern. A module occasionally ships more than one match — e.g.
// gopkg.in/yaml.v2 carries both LICENSE and LICENSE.libyaml (the latter for a
// vendored C dependency) — so the bare "LICENSE" wins when present, otherwise
// the lexicographically first match. [LAW:one-source-of-truth] exactly one
// file is ever treated as "the" license for a module; the tiebreak is fixed
// data (sorted names), not filesystem read order.
func FindLicenseFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read module dir %s: %w", dir, err)
	}

	var matches []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if licenseFilePattern.MatchString(e.Name()) {
			matches = append(matches, e.Name())
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no license file found in %s", dir)
	}
	sort.Strings(matches)

	for _, m := range matches {
		if strings.EqualFold(m, "LICENSE") {
			return filepath.Join(dir, m), nil
		}
	}
	return filepath.Join(dir, matches[0]), nil
}

// Classify identifies the SPDX-ish license name of text using Google's
// licenseclassifier — the same matcher go-licenses uses under the hood —
// returning unclassifiedLicense (never an error) when no known license text
// matches above threshold.
//
// It uses MultipleMatch rather than a single whole-file NearestMatch because
// real LICENSE files routinely aren't ONE license end to end: Apache Software
// Foundation convention appends a BSD/MIT notice for bundled third-party code
// after the project's own Apache-2.0 grant (observed in this tree for
// github.com/apache/thrift and go.opentelemetry.io/otel), and some modules
// concatenate a second project's license they forked from (observed for
// github.com/shopspring/decimal). Scored as one whole-file match, the mixed
// text dilutes every candidate below threshold — MultipleMatch instead finds
// each license at its own offset in the text, each scoring near 1.0. Among
// the matches, the one at the SMALLEST offset is treated as the module's own
// governing license: by the same ASF/fork convention, a module's LICENSE file
// states its own terms first and any bundled third party's terms after.
//
// This calls the classifier library directly rather than shelling out to the
// go-licenses CLI's `save` subcommand, which hard-fails the ENTIRE run the
// moment any single module's license text can't be classified at all
// (observed against github.com/kch42/buzhash's non-standard, profane WTFPL
// wording — exit 1, no bundle produced). That would make this generator's
// exit code depend on the wording of a third party's license file rather
// than on whether our own run produced complete, correct output. Classifying
// inline lets one unclassifiable module become one "Unknown" row instead of a
// failed release build.
func Classify(classifier *lc.License, text string) (name string, confidence float64) {
	matches := classifier.MultipleMatch(text, false)
	if len(matches) == 0 {
		return unclassifiedLicense, 0
	}
	best := matches[0]
	for _, m := range matches[1:] {
		if m.Offset < best.Offset {
			best = m
		}
	}
	if !classifier.WithinConfidenceThreshold(best.Confidence) {
		return unclassifiedLicense, 0
	}
	return best.Name, best.Confidence
}
