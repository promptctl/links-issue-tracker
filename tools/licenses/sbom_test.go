package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// wireBOM is a consumer's-eye view of the encoded SBOM: only the fields a
// CycloneDX validator or a vulnerability scanner reads. Decoding the emitted
// bytes into this (rather than round-tripping through cyclonedx-go's own
// decoder) asserts the WIRE CONTRACT — what actually lands in the release
// asset — not the library's in-memory shape. [LAW:behavior-not-structure]
type wireBOM struct {
	BOMFormat    string `json:"bomFormat"`
	SpecVersion  string `json:"specVersion"`
	SerialNumber string `json:"serialNumber"`
	Metadata     struct {
		Timestamp string `json:"timestamp"`
		Component struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"component"`
	} `json:"metadata"`
	Components []struct {
		Type        string `json:"type"`
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
		PURL        string `json:"purl"`
		Licenses    []struct {
			License struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				Acknowledgement string `json:"acknowledgement"`
			} `json:"license"`
			// CycloneDX's sibling arm for a compound SPDX grant. Decoded here
			// so a test can prove which arm a license landed in: a scanner
			// reading one arm sees nothing at all of a value filed in the
			// other, so "it serialized" is not the contract — "it serialized
			// where SPDX-aware consumers look" is.
			Expression string `json:"expression"`
			// Set on a row lit concluded rather than read off a notice, so a
			// consumer can tell an election from a contradiction of whatever
			// grant it resolves for the coordinate on its own.
			Acknowledgement string `json:"acknowledgement"`
		} `json:"licenses"`
	} `json:"components"`
}

// synthEntries is a fixed, offline inventory covering the cases the SBOM
// renderer must get right: a normal module, a `+incompatible` version whose
// `+` must be percent-encoded in the purl, and an unclassified license that
// must NOT become a fabricated license entry.
// PackageURL is set exactly as buildEntries would set it (goModulePURL), since
// buildSBOM now reads the precomputed field rather than recomputing it.
var synthEntries = []Entry{
	{Module: Module{Path: "github.com/dolthub/dolt/go", Version: "v0.40.5"}, LicenseName: "Apache-2.0", PackageURL: goModulePURL("github.com/dolthub/dolt/go", "v0.40.5")},
	{Module: Module{Path: "github.com/aliyun/aliyun-oss-go-sdk", Version: "v3.0.2+incompatible"}, LicenseName: "MIT", PackageURL: goModulePURL("github.com/aliyun/aliyun-oss-go-sdk", "v3.0.2+incompatible")},
	{Module: Module{Path: "example.com/mystery", Version: "v1.0.0"}, LicenseName: unclassifiedLicense, PackageURL: goModulePURL("example.com/mystery", "v1.0.0")},
}

func decodeSBOM(t *testing.T, entries []Entry, appVersion string) wireBOM {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteSBOM(&buf, entries, appVersion); err != nil {
		t.Fatalf("WriteSBOM: %v", err)
	}
	var bom wireBOM
	if err := json.Unmarshal(buf.Bytes(), &bom); err != nil {
		t.Fatalf("emitted SBOM is not valid JSON: %v\n%s", err, buf.String())
	}
	return bom
}

// TestWriteSBOMWireContract pins the top-level CycloneDX identity fields plus
// the per-component fields a scanner keys on. specVersion MUST be "1.6" (the
// version cyclonedx-cli validates and the value sbomSpecVersion selects) even
// though cyclonedx-go builds a 1.7 document in memory — a regression to 1.7
// would fail the release workflow's validate step, so it fails here first.
func TestWriteSBOMWireContract(t *testing.T) {
	bom := decodeSBOM(t, synthEntries, "1.2.3")

	if bom.BOMFormat != "CycloneDX" {
		t.Errorf("bomFormat = %q, want CycloneDX", bom.BOMFormat)
	}
	if bom.SpecVersion != "1.6" {
		t.Errorf("specVersion = %q, want 1.6 (the version cyclonedx-cli validates)", bom.SpecVersion)
	}
	if !strings.HasPrefix(bom.SerialNumber, "urn:uuid:") {
		t.Errorf("serialNumber = %q, want a urn:uuid:", bom.SerialNumber)
	}
	if bom.Metadata.Timestamp == "" {
		t.Error("metadata.timestamp is empty")
	}
	if bom.Metadata.Component.Name != "lit" || bom.Metadata.Component.Version != "1.2.3" {
		t.Errorf("metadata.component = %+v, want name lit version 1.2.3", bom.Metadata.Component)
	}
	if len(bom.Components) != len(synthEntries) {
		t.Fatalf("got %d components, want %d", len(bom.Components), len(synthEntries))
	}
}

// TestWriteSBOMComponentFields pins the exact per-component rendering: purl
// format (including `+` percent-encoding), that a classified license lands in
// the `name` field (never `id`, which the 1.6 schema restricts to the SPDX
// enum), and that an unclassified module contributes NO license rather than a
// fabricated "Unknown".
func TestWriteSBOMComponentFields(t *testing.T) {
	bom := decodeSBOM(t, synthEntries, "1.2.3")

	byName := map[string]int{}
	for i, c := range bom.Components {
		byName[c.Name] = i
	}

	dolt := bom.Components[byName["github.com/dolthub/dolt/go"]]
	if dolt.Type != "library" {
		t.Errorf("dolt component type = %q, want library", dolt.Type)
	}
	if dolt.Version != "v0.40.5" {
		t.Errorf("dolt version = %q, want v0.40.5", dolt.Version)
	}
	if dolt.PURL != "pkg:golang/github.com/dolthub/dolt/go@v0.40.5" {
		t.Errorf("dolt purl = %q, want pkg:golang/github.com/dolthub/dolt/go@v0.40.5", dolt.PURL)
	}
	if len(dolt.Licenses) != 1 || dolt.Licenses[0].License.Name != "Apache-2.0" {
		t.Errorf("dolt licenses = %+v, want a single license with name Apache-2.0", dolt.Licenses)
	}
	if dolt.Licenses[0].License.ID != "" {
		t.Errorf("dolt license.id = %q, want empty (name, not id, is used)", dolt.Licenses[0].License.ID)
	}
	if dolt.Licenses[0].Expression != "" {
		t.Errorf("dolt license expression = %q, want empty — a single SPDX id belongs in license.name, not the expression arm", dolt.Licenses[0].Expression)
	}

	ali := bom.Components[byName["github.com/aliyun/aliyun-oss-go-sdk"]]
	if ali.PURL != "pkg:golang/github.com/aliyun/aliyun-oss-go-sdk@v3.0.2%2Bincompatible" {
		t.Errorf("aliyun purl = %q, want the `+` percent-encoded as %%2B", ali.PURL)
	}

	mystery := bom.Components[byName["example.com/mystery"]]
	if len(mystery.Licenses) != 0 {
		t.Errorf("unclassified module got licenses %+v, want none (no fabricated Unknown)", mystery.Licenses)
	}
}

// TestSBOMLicenseArmsAreExclusive pins CycloneDX 1.6's `oneOf` on each element
// of licenses[]: a choice carries EITHER a `license` object OR an `expression`
// string, never both and never neither. The schema enforces this, but
// cyclonedx-cli only runs on the release path — by design, to keep an external
// pinned-binary download off the PR critical path — so a change that populated
// both arms would first surface at tag-cut time, after the ephemeral-tag build.
// Decoding into generic maps rather than wireBOM is what makes the check real:
// a hand-written struct cannot tell an absent `license` key from a zero-valued
// one, which is exactly the distinction the invariant is about.
// [LAW:verifiable-goals] this is the offline proof of the document shape the
// release validator would otherwise be the first to see.
func TestSBOMLicenseArmsAreExclusive(t *testing.T) {
	entries, err := buildEntries(litPkg)
	if err != nil {
		t.Fatalf("buildEntries(%s): %v", litPkg, err)
	}
	var buf bytes.Buffer
	if err := WriteSBOM(&buf, entries, "9.9.9"); err != nil {
		t.Fatalf("WriteSBOM: %v", err)
	}
	var doc struct {
		Components []map[string]any `json:"components"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("emitted SBOM is not valid JSON: %v", err)
	}

	checked := 0
	for _, c := range doc.Components {
		raw, ok := c["licenses"]
		if !ok {
			continue
		}
		choices, ok := raw.([]any)
		if !ok {
			t.Fatalf("component %v: licenses is %T, want an array", c["name"], raw)
		}
		if len(choices) != 1 {
			t.Errorf("component %v: %d license choices, want exactly 1", c["name"], len(choices))
		}
		for _, ch := range choices {
			choice, ok := ch.(map[string]any)
			if !ok {
				t.Fatalf("component %v: license choice is %T, want an object", c["name"], ch)
			}
			_, hasLicense := choice["license"]
			_, hasExpression := choice["expression"]
			if hasLicense == hasExpression {
				t.Errorf("component %v: license choice has license=%v expression=%v, want exactly one arm: %v",
					c["name"], hasLicense, hasExpression, choice)
			}
			// The acknowledgement is arm-dependent in the same `oneOf`, and
			// getting it wrong is invisible to any struct-shaped test. The name
			// arm permits NO key but `license` (additionalProperties: false), so
			// an acknowledgement hoisted to choice level there is schema-invalid
			// even though it round-trips through Go without complaint; on the
			// expression arm the same key is legal. Key presence in the raw map
			// is the only way to state that difference.
			if _, ackAtChoiceLevel := choice["acknowledgement"]; hasLicense && ackAtChoiceLevel {
				t.Errorf("component %v: acknowledgement sits beside `license`, which CycloneDX 1.6 forbids — it belongs inside the license object: %v",
					c["name"], choice)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no license choices found in the SBOM; the invariant went unchecked")
	}
}

// TestWriteSBOMOmitsEmptyAppVersion pins the snapshot-gate path, where the
// generator runs with -app-version "" (steps.kind sets a version only for a
// pending release). The subject component must then OMIT the version key
// entirely — not emit "version": "" — which cyclonedx.Component's
// `json:"version,omitempty"` tag already does, and carry a purl with no
// trailing "@". This is the path CI exercises every master push but no other
// Go test covers. Decoding the component into a generic map is what lets the
// test distinguish an absent key from an empty string. [LAW:verifiable-goals]
func TestWriteSBOMOmitsEmptyAppVersion(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteSBOM(&buf, synthEntries, ""); err != nil {
		t.Fatalf("WriteSBOM: %v", err)
	}
	var doc struct {
		Metadata struct {
			Component map[string]any `json:"component"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("emitted SBOM is not valid JSON: %v", err)
	}
	comp := doc.Metadata.Component
	if comp["name"] != "lit" {
		t.Errorf("metadata.component name = %v, want lit", comp["name"])
	}
	if v, ok := comp["version"]; ok {
		t.Errorf("empty appVersion must omit the version key, but component carries version = %v", v)
	}
	if got := comp["purl"]; got != "pkg:golang/github.com/promptctl/links-issue-tracker" {
		t.Errorf("empty-version subject purl = %v, want pkg:golang/github.com/promptctl/links-issue-tracker (no trailing @)", got)
	}
}

// TestBuildSBOMDeterministic pins reproducibility: for a fixed inventory and
// fixed serial + timestamp, the encoded document is byte-identical run to run.
// The two genuinely-varying fields (serial, timestamp) are parameters here, so
// only real inventory changes can change the output — the property that makes
// a released SBOM diffable across versions. [LAW:effects-at-boundaries]
func TestBuildSBOMDeterministic(t *testing.T) {
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	const serial = "urn:uuid:00000000-0000-0000-0000-000000000000"

	encode := func() []byte {
		bom := buildSBOM(synthEntries, "1.2.3", serial, ts)
		var buf bytes.Buffer
		if err := encodeSBOM(&buf, bom); err != nil {
			t.Fatalf("encode: %v", err)
		}
		return buf.Bytes()
	}

	if !bytes.Equal(encode(), encode()) {
		t.Error("two encodings of the same fixed inventory differ; SBOM is not reproducible")
	}
}

// TestSBOMEndToEndCoversDolt is this ticket's acceptance criterion as a fast,
// offline-free-of-cyclonedx-cli test: generate the SBOM from the REAL linked
// module set and confirm it lists github.com/dolthub/dolt at the pinned
// version resolved from the build, with a matching purl. The release workflow
// additionally runs `cyclonedx validate` against the shipped file; this proves
// the content contract without that external tool.
func TestSBOMEndToEndCoversDolt(t *testing.T) {
	entries, err := buildEntries(litPkg)
	if err != nil {
		t.Fatalf("buildEntries(%s): %v", litPkg, err)
	}
	bom := decodeSBOM(t, entries, "9.9.9")

	var doltWant string
	for _, e := range entries {
		if e.Module.Path == "github.com/dolthub/dolt/go" {
			doltWant = e.Module.Version
		}
	}
	if doltWant == "" {
		t.Fatal("github.com/dolthub/dolt/go not among linked modules")
	}

	for _, c := range bom.Components {
		if c.Name != "github.com/dolthub/dolt/go" {
			continue
		}
		if c.Version != doltWant {
			t.Errorf("dolt version in SBOM = %q, want %q", c.Version, doltWant)
		}
		if want := "pkg:golang/github.com/dolthub/dolt/go@" + doltWant; c.PURL != want {
			t.Errorf("dolt purl = %q, want %q", c.PURL, want)
		}
		return
	}
	t.Fatalf("github.com/dolthub/dolt/go not found among %d SBOM components", len(bom.Components))
}
