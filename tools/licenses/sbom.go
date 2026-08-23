package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/google/uuid"
	packageurl "github.com/package-url/packageurl-go"
)

// sbomSpecVersion is the CycloneDX spec version the SBOM is emitted at. 1.6 is
// the newest version cyclonedx-cli (the validator the release workflow runs
// against the shipped file) accepts; cyclonedx-go defaults a fresh BOM to 1.7,
// so buildSBOM must convert down at encode time or a 1.7 document would fail
// the very `cyclonedx validate` that is this ticket's acceptance check.
// [LAW:one-source-of-truth] the emitted spec version lives here only; the
// encoder call and any validator pin both read this single value.
const sbomSpecVersion = cdx.SpecVersion1_6

// litModulePath is the subject the SBOM describes: this repository's own
// module. It becomes metadata.component so a reader of the standalone SBOM
// asset can tell what the bill of materials is *of*, not just an unlabeled
// list of dependencies. [LAW:one-source-of-truth] the module path is stated
// once; the purl below is derived from it.
const litModulePath = "github.com/promptctl/links-issue-tracker"

// WriteSBOM renders the linked-module inventory as a CycloneDX SBOM (JSON) to
// w. It is the effectful boundary: it mints the per-document serial number and
// timestamp — the two fields that legitimately vary run to run — and hands
// them to the pure buildSBOM so the document's *structure* stays a
// deterministic function of entries. [LAW:effects-at-boundaries] no clock or
// randomness reaches the builder; tests drive buildSBOM directly with fixed
// values.
func WriteSBOM(w io.Writer, entries []Entry, appVersion string) error {
	serial := "urn:uuid:" + uuid.NewString()
	bom := buildSBOM(entries, appVersion, serial, time.Now().UTC())
	return encodeSBOM(w, bom)
}

// encodeSBOM writes bom as pretty CycloneDX JSON at sbomSpecVersion. It is the
// single home for the encoder settings so WriteSBOM (production) and the
// determinism test encode identically. EncodeVersion converts the in-memory
// 1.7 BOM down to sbomSpecVersion before writing — plain Encode would emit
// specVersion 1.7 and fail cyclonedx-cli, the validator this file must satisfy.
// [LAW:one-source-of-truth]
func encodeSBOM(w io.Writer, bom *cdx.BOM) error {
	enc := cdx.NewBOMEncoder(w, cdx.BOMFileFormatJSON)
	enc.SetPretty(true)
	if err := enc.EncodeVersion(bom, sbomSpecVersion); err != nil {
		return fmt.Errorf("encode CycloneDX SBOM: %w", err)
	}
	return nil
}

// buildSBOM assembles the CycloneDX document from the same entries the bundle
// and report render — one module, one component, in the order LinkedModules
// already fixed. [LAW:one-source-of-truth] entries carries the canonical
// module set and its order; this function neither re-resolves nor re-sorts it,
// so the SBOM provably describes the identical set the license report does.
//
// serialNumber and timestamp are parameters, not computed here, so the whole
// function is pure and its output is byte-deterministic for a given inventory
// — the property sbom_test.go asserts against.
func buildSBOM(entries []Entry, appVersion, serialNumber string, timestamp time.Time) *cdx.BOM {
	components := make([]cdx.Component, 0, len(entries))
	for _, e := range entries {
		components = append(components, cdx.Component{
			Type:    cdx.ComponentTypeLibrary,
			Name:    e.Module.Path,
			Version: e.Module.Version,
			// What the component IS, which is what CycloneDX defines this field
			// to hold. The curated licensing note is a different kind of claim
			// and travels in Properties below; see licenseNoteProperty.
			Description: e.Description,
			PackageURL:  e.PackageURL,
			Licenses:    componentLicenses(e.LicenseName, e.Acknowledgement),
			Properties:  componentProperties(e.Note),
			Pedigree:    componentPedigree(e.Module.Replacement),
		})
	}

	bom := cdx.NewBOM()
	bom.SerialNumber = serialNumber
	bom.Metadata = &cdx.Metadata{
		Timestamp: timestamp.Format(time.RFC3339),
		Component: &cdx.Component{
			Type:       cdx.ComponentTypeApplication,
			Name:       "lit",
			Version:    appVersion,
			PackageURL: goModulePURL(litModulePath, appVersion),
		},
	}
	bom.Components = &components
	return bom
}

// goModulePURL builds the Package URL for a Go module — the field vulnerability
// scanners key on to match a component against CVE/advisory feeds, which is the
// whole reason a machine-readable SBOM earns its place. It delegates the
// escaping rules (a `+incompatible` version, say, must percent-encode the `+`)
// to packageurl-go rather than string-concatenating a purl by hand, so the
// output is spec-correct by construction. [LAW:types-are-the-program] the purl
// library owns the format; we only supply the parts.
//
// Go's purl type splits the module path into namespace (everything up to the
// last segment) and name (the last segment): github.com/dolthub/dolt/go ->
// namespace github.com/dolthub/dolt, name go.
func goModulePURL(modulePath, version string) string {
	namespace, name := modulePath, ""
	if i := strings.LastIndex(modulePath, "/"); i >= 0 {
		namespace, name = modulePath[:i], modulePath[i+1:]
	} else {
		namespace, name = "", modulePath
	}
	return packageurl.NewPackageURL(packageurl.TypeGolang, namespace, name, version, nil, "").ToString()
}

// licenseNoteProperty is the namespaced name under which a curated licensing
// note travels in the SBOM. It is deliberately NOT component.description:
// CycloneDX defines description as what the component IS, and four sentences
// explaining a dual-license election are not that. The distinction is not
// pedantry — merge and dedup tooling treats description as identity metadata,
// so a note filed there is a note that can be matched against, truncated, or
// overwritten as though it named the component.
//
// The name is namespaced to this project because CycloneDX property names are a
// flat global space with no schema behind them; an unprefixed "license-note"
// would collide with anyone else's. Nothing is lost by moving the note out of
// description: it still ships verbatim in LICENSE-REPORT.md's Notes section and
// in THIRD_PARTY_LICENSES, which are the documents a human actually reads.
const licenseNoteProperty = "promptctl:lit:license-note"

// componentProperties renders a curated note as the component's property list,
// or nil when there is no note — which omitempty then drops from the JSON
// entirely, so the 149 note-free Go modules emit no empty array.
// [LAW:dataflow-not-control-flow] the absence is a value this returns, so
// buildSBOM sets the field unconditionally rather than branching around it.
func componentProperties(note string) *[]cdx.Property {
	if note == "" {
		return nil
	}
	properties := []cdx.Property{{Name: licenseNoteProperty, Value: note}}
	return &properties
}

// pedigreeNoteFork explains a fork-shaped substitution in the words the
// structured fields cannot. `descendants` states a genealogy — that a fork of
// this component exists — and nothing more; it does not say that the fork is
// what lit actually compiled, which is the whole fact this ticket exists to
// disclose.
const pedigreeNoteFork = "lit's go.mod requires this coordinate, but a replace directive substitutes its source: " +
	"the code compiled into lit came from the fork recorded under descendants, not from the version and purl above. " +
	"Both coordinates resolve publicly; the fork is the one to fetch when diffing against a lit build. " +
	"The modifications and the reasons for them are catalogued in FORKS.md, in lit's source repository at " +
	litModulePath + "."

// pedigreeNoteVersion covers `replace x => x v1.2.3`, where the substitute is
// the SAME module at another version. It records NO descendant: the component
// did not fork, and a descendants entry whose purl differs from the component's
// only in its version would assert that the module descends from itself.
const pedigreeNoteVersion = "lit's go.mod requires this coordinate at the version above, but a replace directive " +
	"substitutes the same module at version %s, which is the code compiled into lit. The module path is unchanged " +
	"and no fork is involved, so no descendant component is recorded."

// pedigreeNoteDirectory is the directory-shaped counterpart. It must state why
// there is no component recorded beside it: an absent descendants list is
// otherwise indistinguishable from an omission, and a reader who takes it for
// one learns the opposite of the truth — that nothing was substituted.
// It also claims no containment. modfile.IsDirectoryPath accepts `../sibling`
// and absolute paths as readily as `./internal/...`, so "carried inside lit's
// own repository" — which this string said until bundle.go was corrected and
// this twin was not — is false for a sibling checkout or /opt/src. Two
// renderers stating one fact is exactly how one of them comes to state it
// wrongly. [LAW:one-source-of-truth]
const pedigreeNoteDirectory = "lit's go.mod requires this coordinate, but a replace directive substitutes its source with %s, " +
	"a patched local directory. The code compiled into lit came from there, not from the version " +
	"and purl above. No descendant component is recorded because no published coordinate identifies the patched source, " +
	"and a purl invented for it would name something no consumer could resolve."

// componentPedigree records that a component's source came from somewhere other
// than the coordinate naming it — CycloneDX's field for exactly the fidelity
// gap FORKS.md documents, where three components' license rows are all correct
// while the artifacts describe source the binary does not contain.
//
// The component keeps its go.mod identity rather than being renamed to the
// substitute. That is the load-bearing choice here, and it is not the fork
// idiom CycloneDX's own prose sketches (component = the fork, ancestor = the
// original). Renaming would break two things that matter more: the invariant
// that the SBOM and LICENSE-REPORT.md provably describe ONE inventory (see
// main.go), since every other artifact keys on the go.mod path; and purl-based
// vulnerability matching, because no advisory feed carries the fork's
// coordinate. So identity answers "what does lit depend on" and the pedigree
// answers "what did lit compile", and the document states both.
//
// A fork — and ONLY a fork — earns a descendants entry. `replace x => x v1.2.3`
// substitutes the same module at another version, which is a substitution worth
// disclosing but not a genealogy; recording it as a descendant would have the
// document assert that a component descends from itself.
//
// Given that identity, the fork belongs in `descendants` and NOT in
// `ancestors`, and the difference is a true statement versus a false one.
// CycloneDX defines descendants as the forks of an original component, which is
// exactly what github.com/promptctl/dolt/go is with respect to the coordinate
// this component names. Filing it under ancestors — "the component this one was
// derived from" — would assert that dolthub/dolt derives from promptctl's fork,
// reversing the real genealogy in a structured field a machine reads without
// the notes beside it. Shipping a true-sounding claim in a field that means
// something else is the defect this ticket was opened to remove, not one to
// re-commit while removing it.
//
// [LAW:dataflow-not-control-flow] the single switch is the domain's own
// discriminator — the three shapes admit different facts, and each arm says
// exactly what its shape knows — not a special case carved into the renderer.
func componentPedigree(r Replacement) *cdx.Pedigree {
	switch r.Kind {
	case NotReplaced:
		return nil
	case ReplacedByFork:
		descendants := []cdx.Component{{
			Type:       cdx.ComponentTypeLibrary,
			Name:       r.Path,
			Version:    r.Version,
			PackageURL: goModulePURL(r.Path, r.Version),
		}}
		return &cdx.Pedigree{Descendants: &descendants, Notes: pedigreeNoteFork}
	case ReplacedByVersion:
		return &cdx.Pedigree{Notes: fmt.Sprintf(pedigreeNoteVersion, r.Version)}
	case ReplacedByDirectory:
		return &cdx.Pedigree{Notes: fmt.Sprintf(pedigreeNoteDirectory, r.Path)}
	default:
		panic(fmt.Sprintf(unhandledReplacementKind, r.Kind, r.Path))
	}
}

// componentLicenses renders a classified license name as a CycloneDX license
// choice, or nil when the module's license could not be classified. An
// unclassified module contributes a component with no license rather than a
// fabricated one; the full text and the "Unknown" row still travel in
// THIRD_PARTY_LICENSES and LICENSE-REPORT.md, so nothing is hidden.
// [LAW:no-silent-failure]
//
// It tests unclassifiedLicense specifically and NOT the licenseSentinels set,
// which is the opposite of what the policy rules do, because the domains
// differ: Entry.LicenseName is written in exactly two places — Classify, which
// returns a corpus name or unclassifiedLicense, and native.go's four curated
// literals. oversizeLicense is produced only by the graph scanner, into
// LicenseHit, and never becomes an Entry. Widening this condition to the set
// read as tidier and added a branch nothing can reach; it was the one rule in
// this package whose mutation survived the whole suite, which is how it was
// caught.
func componentLicenses(name, acknowledgement string) *cdx.Licenses {
	if name == "" || name == unclassifiedLicense {
		return nil
	}
	licenses := cdx.Licenses{licenseChoice(name, acknowledgement)}
	return &licenses
}

// acknowledgementConcluded marks a license row lit ARRIVED AT rather than one
// it read off a license file — zstd, where upstream offers BSD-3-Clause OR
// GPL-2.0-only and lit elected the first, and compiler-rt, whose expression was
// composed from the ported LLVM sources' own headers. CycloneDX defines the
// field for exactly this, and without it lit's single BSD-3-Clause row reads to
// a scanner that independently resolves zstd 1.5.6 as a contradiction of the
// dual grant rather than as a choice between its arms. Rows left unset make no
// claim either way: for the classified Go modules lit never established that
// the LICENSE file it read is upstream's authoritative declaration, and
// asserting `declared` across all of them would be a stronger statement than
// the classifier earns.
const acknowledgementConcluded = "concluded"

// ackPtr is the pointer form LicenseChoice's field wants — nil when the row
// makes no acknowledgement claim, which omitempty then drops from the JSON.
// [LAW:dataflow-not-control-flow] the unstated case is a value this returns,
// so licenseChoice sets the field unconditionally instead of branching on
// whether to set it.
func ackPtr(acknowledgement string) *cdx.LicenseAcknowledgement {
	if acknowledgement == "" {
		return nil
	}
	ack := cdx.LicenseAcknowledgement(acknowledgement)
	return &ack
}

// spdxOperators are the three operators of the SPDX license-expression
// grammar. A license value is compound exactly when one of them appears as a
// whole space-delimited token: SPDX identifiers never contain a space, so an
// operator can only ever stand as its own field. Matching tokens rather than
// substrings is what keeps an identifier that merely spells an operator inside
// itself — GPL-2.0-or-later, Apache-2.0 — on the single-name arm by
// construction rather than by luck. That matters because LicenseName is not a
// curated set: it also carries whatever licenseclassifier returns for every Go
// module in the link closure.
var spdxOperators = map[string]bool{"AND": true, "OR": true, "WITH": true}

// licenseChoice maps one license value onto the CycloneDX arm that represents
// it. CycloneDX models the two shapes as a sum — a single named license, or an
// SPDX expression — and this is the one place lit's license strings are mapped
// onto it, so no renderer re-derives which arm a value belongs in.
// [LAW:parse-dont-validate] the returned LicenseChoice is the proof of WHICH
// ARM, and of nothing else: it does not establish that an expression-valued
// string is well-formed SPDX, and no caller should read it that way. Grammar
// conformance of the four curated strings in native.go is held by review plus
// the exact-match against policy.json's allowlist, which a typo would have to
// be replicated into before the gate would pass.
//
// The `name` field is used, never `id`: the 1.6 schema constrains license.id to
// the SPDX id enum, so a classifier output that isn't an exact SPDX id placed
// in `id` would fail the very validation this SBOM must pass. An expression has
// no such constraint — `expression` is where the schema puts compound grants,
// and a compound left in `name` reads to a scanner as one license literally
// named with an AND in it.
func licenseChoice(name, acknowledgement string) cdx.LicenseChoice {
	// [LAW:dataflow-not-control-flow] the one branch is CycloneDX's own sum
	// type, which is the entire point of the discriminator — not a special
	// case carved into the renderer. Both arms carry the acknowledgement, but
	// on DIFFERENT fields that are not interchangeable: the 1.6 schema's name
	// arm is additionalProperties:false permitting `license` and nothing else,
	// so an acknowledgement hoisted beside `license` is schema-invalid, while
	// on the expression arm it is a permitted sibling of `expression`. Do not
	// unify the two onto ackPtr — it compiles, it round-trips, and it fails
	// validation at tag-cut.
	if slices.ContainsFunc(strings.Fields(name), func(field string) bool { return spdxOperators[field] }) {
		return cdx.LicenseChoice{Expression: name, Acknowledgement: ackPtr(acknowledgement)}
	}
	return cdx.LicenseChoice{License: &cdx.License{
		Name:            name,
		Acknowledgement: cdx.LicenseAcknowledgement(acknowledgement),
	}}
}
