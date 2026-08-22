package main

import (
	"fmt"
	"io"
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
			// A curated note (dual-license election, compound-expression
			// provenance) rides as the component description so the SBOM
			// explains its own license claim; empty for classified Go modules
			// and omitted from the JSON. [LAW:dataflow-not-control-flow]
			Description: e.Note,
			PackageURL:  e.PackageURL,
			Licenses:    componentLicenses(e.LicenseName),
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

// componentLicenses renders a classified license name as a CycloneDX license
// choice, or nil when the module's license could not be classified. It uses
// the `name` field, never `id`: the 1.6 schema constrains license.id to the
// SPDX id enum, so a classifier output that isn't an exact SPDX id (or the
// unclassifiedLicense sentinel) placed in `id` would fail validation — the
// exact check this SBOM must pass. An unclassified module contributes a
// component with no license rather than a fabricated one; the full text and
// the "Unknown" row still travel in THIRD_PARTY_LICENSES and LICENSE-REPORT.md,
// so nothing is hidden. [LAW:no-silent-failure]
func componentLicenses(name string) *cdx.Licenses {
	if name == "" || name == unclassifiedLicense {
		return nil
	}
	licenses := cdx.Licenses{{License: &cdx.License{Name: name}}}
	return &licenses
}
