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
	Components []wireComponent `json:"components"`
}

// wireComponent is one entry of the SBOM's component list, as it lands on the
// wire. It is a named type rather than an anonymous struct inside wireBOM so
// helpers and table-driven tests can pass a single component around without
// restating its shape — a restatement being one more copy to drift.
// [LAW:one-source-of-truth]
type wireComponent struct {
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
	// The curated licensing note's home, as of the split that took it out of
	// description. Decoded as the flat name/value list CycloneDX defines rather
	// than as a map, because a map would silently collapse a duplicate name —
	// and TestSBOMPropertyNamesAreUnique below asserts there is exactly one, an
	// assertion a map decode would make unwritable.
	Properties []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"properties"`
	// Where the source came from when a `replace` directive means it is not the
	// coordinate above. The fork is decoded as a full component rather than a
	// bare name so a test can prove it is RESOLVABLE — carrying the purl a
	// reader would actually fetch — and not merely present.
	Pedigree struct {
		Descendants []struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Version string `json:"version"`
			PURL    string `json:"purl"`
		} `json:"descendants"`
		Notes string `json:"notes"`
	} `json:"pedigree"`
}

// property returns the value of the named CycloneDX property on a component,
// and whether it was present at all. Presence is returned separately because a
// property carrying the empty string and a property never emitted are different
// documents, and tests here assert both cases.
func (c wireComponent) property(name string) (string, bool) {
	for _, p := range c.Properties {
		if p.Name == name {
			return p.Value, true
		}
	}
	return "", false
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

// replacementEntries covers all three `replace` shapes plus an unreplaced control,
// so a disclosure test can prove not only that a substitution is stated but
// that an ordinary module stays silent about one it does not have. It carries
// every field the three renderers read — purl for the SBOM, text for the bundle
// — because all three assert against THIS inventory; three near-identical
// fixtures would let one drift and quietly stop testing the same thing.
// [LAW:one-source-of-truth]
var replacementEntries = []Entry{
	{
		Module: Module{
			Path:    "github.com/dolthub/dolt/go",
			Version: "v0.40.5",
			Replacement: Replacement{
				Kind:    ReplacedByFork,
				Path:    "github.com/promptctl/dolt/go",
				Version: "v0.40.5-later",
			},
		},
		LicenseName: "Apache-2.0",
		PackageURL:  goModulePURL("github.com/dolthub/dolt/go", "v0.40.5"),
		Text:        "Apache text\n",
	},
	{
		Module: Module{
			Path:        "github.com/dolthub/driver",
			Version:     "v0.2.1",
			Replacement: Replacement{Kind: ReplacedByDirectory, Path: "./internal/vendor/dolthub-driver"},
		},
		LicenseName: "Apache-2.0",
		PackageURL:  goModulePURL("github.com/dolthub/driver", "v0.2.1"),
		Text:        "Apache text\n",
	},
	{
		// `replace x => x v1.19.0`: a substitution that must be disclosed and
		// must never be called a fork. It rides in the shared fixture so the
		// bundle and report render it for real, rather than the rule being
		// checked only against the constants in isolation.
		Module: Module{
			Path:    "github.com/spf13/viper",
			Version: "v1.18.0",
			Replacement: Replacement{
				Kind:    ReplacedByVersion,
				Path:    "github.com/spf13/viper",
				Version: "v1.19.0",
			},
		},
		LicenseName: "Apache-2.0",
		PackageURL:  goModulePURL("github.com/spf13/viper", "v1.18.0"),
		Text:        "Apache text\n",
	},
	{
		Module:      Module{Path: "github.com/spf13/cobra", Version: "v1.8.0"},
		LicenseName: "Apache-2.0",
		PackageURL:  goModulePURL("github.com/spf13/cobra", "v1.8.0"),
		Text:        "Apache text\n",
	},
}

// componentsByName indexes a decoded SBOM's components. Every test that looks a
// component up needs it, and each open-coded copy is another place to forget
// that a duplicate name would silently overwrite an earlier entry.
func componentsByName(bom wireBOM) map[string]wireComponent {
	byName := make(map[string]wireComponent, len(bom.Components))
	for _, c := range bom.Components {
		byName[c.Name] = c
	}
	return byName
}

// TestSBOMPedigreeRecordsBothReplacementShapes is links-licensing-c0ce.15's
// acceptance criterion at the unit level: a component whose source came from a
// `replace` directive discloses where it came from, in the CycloneDX field
// defined for it.
//
// The two shapes render DIFFERENTLY on purpose, and the difference is the point
// rather than an inconsistency to tidy away. A module replacement has a
// resolvable coordinate, so it earns a descendant component carrying the purl a
// reader can fetch and diff. A directory replacement has none — its source
// exists only inside lit's own repository — so inventing a purl for it would
// name something no consumer could resolve, and the note has to say why the
// component is missing instead of leaving the absence to be read as an omission.
//
// The fork goes under `descendants`, never `ancestors`: CycloneDX defines
// descendants as the forks OF this component, which promptctl/dolt is, while
// ancestors would assert that dolthub/dolt derives from promptctl's fork and
// reverse the real genealogy in a field machines read without the notes.
func TestSBOMPedigreeRecordsBothReplacementShapes(t *testing.T) {
	byName := componentsByName(decodeSBOM(t, replacementEntries, "1.2.3"))

	t.Run("module replacement carries a resolvable descendant", func(t *testing.T) {
		c := byName["github.com/dolthub/dolt/go"]
		// The component keeps its go.mod identity. That is load-bearing: every
		// other artifact keys on the go.mod path, and renaming the component to
		// the fork would both break the one-inventory invariant and orphan the
		// purl every advisory feed matches against.
		if c.PURL != "pkg:golang/github.com/dolthub/dolt/go@v0.40.5" {
			t.Errorf("replaced component purl = %q, want the go.mod coordinate's purl", c.PURL)
		}
		if len(c.Pedigree.Descendants) != 1 {
			t.Fatalf("pedigree.descendants = %+v, want exactly one fork", c.Pedigree.Descendants)
		}
		d := c.Pedigree.Descendants[0]
		if d.Name != "github.com/promptctl/dolt/go" || d.Version != "v0.40.5-later" {
			t.Errorf("descendant = %s@%s, want github.com/promptctl/dolt/go@v0.40.5-later", d.Name, d.Version)
		}
		// The purl is what makes the entry actionable rather than decorative: it
		// is the string a reader pastes to fetch the source lit compiled.
		if want := "pkg:golang/github.com/promptctl/dolt/go@v0.40.5-later"; d.PURL != want {
			t.Errorf("descendant purl = %q, want %q", d.PURL, want)
		}
		if d.Type != "library" {
			t.Errorf("descendant type = %q, want library — CycloneDX requires a type on every component", d.Type)
		}
		if c.Pedigree.Notes == "" {
			t.Error("pedigree carries no notes; a descendants list states a genealogy and not the fact that the fork is what lit compiled")
		}
	})

	t.Run("directory replacement names the path and says why there is no component beside it", func(t *testing.T) {
		c := byName["github.com/dolthub/driver"]
		if len(c.Pedigree.Descendants) != 0 {
			t.Errorf("directory replacement got descendants %+v, want none — no published coordinate identifies the patched source", c.Pedigree.Descendants)
		}
		if !strings.Contains(c.Pedigree.Notes, "./internal/vendor/dolthub-driver") {
			t.Errorf("pedigree notes do not name the directory the source came from: %q", c.Pedigree.Notes)
		}
		// Without this sentence an absent descendants list is indistinguishable
		// from an omission, and a reader who takes it for one concludes the
		// opposite of the truth: that nothing was substituted.
		if !strings.Contains(c.Pedigree.Notes, "No descendant component is recorded") {
			t.Errorf("pedigree notes do not explain the absent component: %q", c.Pedigree.Notes)
		}
		// The same containment claim bundle.go was corrected to drop. A replace
		// target may be ../sibling or absolute, so the SBOM may not say the
		// copy sits inside this repository either.
		for _, claim := range []string{"inside lit's own repository", "repository-relative"} {
			if strings.Contains(c.Pedigree.Notes, claim) {
				t.Errorf("pedigree notes claim %q, which a ../sibling or absolute replace target would falsify: %q", claim, c.Pedigree.Notes)
			}
		}
	})

	t.Run("a version pin is disclosed but is never called a fork", func(t *testing.T) {
		// `replace x => x v1.2.3` substitutes the same module at another
		// version. It is a real substitution and must be disclosed, but it is
		// NOT a fork — a descendants entry here would have the document assert
		// that a component descends from itself, which is what the separate
		// kind exists to prevent. Asserting the kind alone is not enough: the
		// wrong pedigree is emitted by the renderer, not by the parser.
		pinned := componentPedigree(Replacement{
			Kind:    ReplacedByVersion,
			Path:    "github.com/spf13/viper",
			Version: "v1.19.0",
		})
		if pinned == nil {
			t.Fatal("a version pin got no pedigree; the substitution would ship undisclosed")
		}
		if pinned.Descendants != nil {
			t.Errorf("a version pin got descendants %+v, want none — the module did not fork, and a descendant naming the same path asserts it descends from itself", *pinned.Descendants)
		}
		if !strings.Contains(pinned.Notes, "no fork is involved") {
			t.Errorf("version-pin notes do not disclaim a fork: %q", pinned.Notes)
		}
		if !strings.Contains(pinned.Notes, "v1.19.0") {
			t.Errorf("version-pin notes do not name the substituted version: %q", pinned.Notes)
		}
	})

	t.Run("an unreplaced module carries no pedigree at all", func(t *testing.T) {
		// Asserted on the raw JSON, because the decoded struct cannot tell an
		// absent `pedigree` key from a zero-valued one — and a pedigree object
		// emitted for every component would make the three that matter
		// unfindable.
		var doc struct {
			Components []map[string]any `json:"components"`
		}
		var buf bytes.Buffer
		if err := WriteSBOM(&buf, replacementEntries, "1.2.3"); err != nil {
			t.Fatalf("WriteSBOM: %v", err)
		}
		if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
			t.Fatalf("emitted SBOM is not valid JSON: %v", err)
		}
		for _, c := range doc.Components {
			if c["name"] != "github.com/spf13/cobra" {
				continue
			}
			if p, ok := c["pedigree"]; ok {
				t.Errorf("unreplaced component carries pedigree %v, want the key absent", p)
			}
			return
		}
		t.Fatal("the unreplaced control component is missing from the SBOM")
	})
}

// TestSBOMDisclosesEveryReplacementInTheRealBuild is the guard the ticket asks
// for, run against what lit actually links rather than a fixture: a `replace`
// added to go.mod tomorrow cannot ship as if it were upstream, because the
// component it produces will have no pedigree and this fails.
//
// It is driven from the inventory rather than from a list of the three
// substitutions that exist today. A hardcoded list is a second place to
// remember, and the failure it would miss — a NEW replace — is exactly the one
// worth catching. [LAW:one-source-of-truth]
func TestSBOMDisclosesEveryReplacementInTheRealBuild(t *testing.T) {
	entries, err := buildEntries(litPkg)
	if err != nil {
		t.Fatalf("buildEntries(%s): %v", litPkg, err)
	}
	byName := componentsByName(decodeSBOM(t, entries, "9.9.9"))

	goModForks := forkPURLsFromGoMod(t)
	replaced := 0
	for _, e := range entries {
		c, ok := byName[e.Module.Path]
		if !ok {
			t.Errorf("module %s is in the inventory but has no SBOM component", e.Module.Path)
			continue
		}
		if !e.Module.IsReplaced() {
			if c.Pedigree.Notes != "" || len(c.Pedigree.Descendants) > 0 {
				t.Errorf("%s is not replaced but carries a pedigree: %+v", e.Module.Path, c.Pedigree)
			}
			continue
		}
		replaced++
		if c.Pedigree.Notes == "" {
			t.Errorf("%s is built from %s but its SBOM component discloses nothing; it ships as if it were upstream",
				e.Module.Path, e.Module.Replacement)
		}
		// A module replacement must additionally hand the reader the coordinate
		// to fetch. Checking the kind here rather than the module's name is what
		// keeps this from needing an edit when a fork is added or retired.
		if e.Module.Replacement.Kind == ReplacedByFork {
			if len(c.Pedigree.Descendants) != 1 {
				t.Errorf("%s is replaced by the fork %s but has %d descendants, want 1",
					e.Module.Path, e.Module.Replacement, len(c.Pedigree.Descendants))
				continue
			}
			// Compared against go.mod's OWN replace directive, read here with
			// modfile, rather than against the Replacement the SBOM was built
			// from. Deriving the expectation from the same value production
			// used makes the assertion an identity: it holds even if
			// moduleFields and parseReplacement between them mis-split every
			// coordinate, because both sides would be wrong together.
			want, ok := goModForks[e.Module.Path]
			if !ok {
				t.Errorf("%s is replaced in the linked set but go.mod declares no fork for it", e.Module.Path)
				continue
			}
			if got := c.Pedigree.Descendants[0].PURL; got != want {
				t.Errorf("%s descendant purl = %q, want %q (from go.mod)", e.Module.Path, got, want)
			}
		}
	}

	// lit's go.mod carries three `replace` directives today. A zero here would
	// mean the test walked an inventory that no longer reports substitutions at
	// all — a passing test that proves nothing, which is the failure mode this
	// whole ticket is about. [LAW:no-silent-failure]
	if replaced == 0 {
		t.Fatal("no replaced modules found in the linked set; the disclosure went unverified")
	}
}

// TestSBOMPedigreeRefusesAnUnknownKind is componentPedigree's half of the
// exhaustiveness guarantee. String()'s arm is tested next door; this one
// matters independently because a pedigree silently omitted is a component that
// ships as if it were upstream — the exact defect this ticket removed.
func TestSBOMPedigreeRefusesAnUnknownKind(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("componentPedigree returned normally for an unhandled ReplacementKind; the substitution would go undisclosed")
		}
	}()
	_ = componentPedigree(Replacement{Kind: ReplacementKind(99), Path: "github.com/example/whatever"})
}

// TestSBOMPropertyNamesAreUnique pins what wireComponent's list-shaped decode of
// `properties` exists to make assertable. CycloneDX permits repeated property
// names, and every reader in this package — c.property(), and any consumer
// keying a map off the name — takes the FIRST match, so a second
// promptctl:lit:license-note carrying a contradictory value would ship in the
// document while every other test stayed green.
func TestSBOMPropertyNamesAreUnique(t *testing.T) {
	// Driven from the curated native inventory rather than a real build. Only
	// those four components can populate Properties at all, and buildEntries
	// costs a `go list -deps` plus 149 license classifications (~2s) to reach
	// the same four.
	checked := 0
	for _, c := range decodeSBOM(t, nativeEntries(), "9.9.9").Components {
		seen := map[string]int{}
		for _, p := range c.Properties {
			seen[p.Name]++
			checked++
		}
		for name, n := range seen {
			if n != 1 {
				t.Errorf("component %s carries %d properties named %q; readers take the first and the rest ship unread", c.Name, n, name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no properties found in the SBOM; the uniqueness invariant went unchecked")
	}
}

// forkPURLsFromGoMod reads the repository's own go.mod and returns, for every
// replaced module path, the purl of the coordinate go.mod substitutes it with.
//
// It exists so the real-build pedigree guard can compare the SBOM against an
// INDEPENDENT reading of the same fact. `go list` and modfile are two different
// paths to the replace directives, so a defect anywhere between the go list
// template and parseReplacement shows up as a disagreement rather than
// cancelling out. The go.mod read itself goes through forks_test.go's
// parseRootGoMod: "independent of go list" is the point, and a second private
// copy of "how to read this repo's go.mod" would not add independence, only
// drift. [LAW:one-source-of-truth]
func forkPURLsFromGoMod(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, r := range parseRootGoMod(t).Replace {
		if r.New.Version == "" || r.New.Path == r.Old.Path {
			continue // a directory target or a version pin: no fork purl to expect
		}
		out[r.Old.Path] = goModulePURL(r.New.Path, r.New.Version)
	}
	return out
}
