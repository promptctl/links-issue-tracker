package storage

import (
	"strings"
	"testing"
)

// The authored-file parsers' own tests: bytes in, specs or a named refusal out,
// with no engine involved. They live here rather than beside an engine because
// the schema is the contract's — every engine reads the same authored file, so
// what the file is allowed to say is settled once, here.

func TestParseBulkSpecsRejectsUnknownField(t *testing.T) {
	t.Parallel()
	doc := []byte("title: X\ntopic: bulk\ntype: task\nchildren: [a, b]\n")
	if _, err := ParseBulkSpecs(doc); err == nil || !strings.Contains(err.Error(), "children") {
		t.Fatalf("ParseBulkSpecs(unknown field) error = %v, want error naming \"children\"", err)
	}
}

func TestParseBulkSpecsMultiDocument(t *testing.T) {
	t.Parallel()
	doc := []byte("title: A\ntopic: bulk\ntype: task\n---\ntitle: B\ntopic: bulk\ntype: task\n")
	specs, err := ParseBulkSpecs(doc)
	if err != nil {
		t.Fatalf("ParseBulkSpecs() error = %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("len(specs) = %d, want 2", len(specs))
	}
}

// A nested spec — hierarchy via a "children" array instead of the flat
// local_id+parent form — is the canonical schema-drift case. The unknown
// "children" field must be rejected by name, not silently dropped.
func TestParseImportTreeSpecsRejectsUnknownField(t *testing.T) {
	t.Parallel()
	nested := []byte(`[{"local_id":"e1","title":"Epic","type":"epic","topic":"x","children":[{"local_id":"t1","title":"Child"}]}]`)
	_, err := ParseImportTreeSpecs(nested)
	if err == nil || !strings.Contains(err.Error(), "children") {
		t.Fatalf("ParseImportTreeSpecs(nested) error = %v, want error naming \"children\"", err)
	}
}

func TestParseImportTreeSpecsRejectsTrailingData(t *testing.T) {
	t.Parallel()
	doc := []byte(`[{"local_id":"a","title":"A","type":"task","topic":"x"}] [{"local_id":"b","title":"B","type":"task","topic":"x"}]`)
	_, err := ParseImportTreeSpecs(doc)
	if err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("ParseImportTreeSpecs(trailing) error = %v, want trailing-data error", err)
	}
}
