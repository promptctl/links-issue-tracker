package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// The authored-file parsers for the bulk and tree-import schemas.
//
// They live beside the specs they produce rather than in an engine, because
// the schema is the contract's: [BulkIssueSpec] and [ImportTreeSpec] are what
// [BulkWriter] consumes, so the bytes-to-spec crossing belongs wherever the
// spec is defined and every engine reads the same authored file the same way.
// [LAW:single-enforcer]

// ParseBulkSpecs is the deserialization trust boundary for bulk-input files:
// raw YAML bytes in, one spec per document out. It rejects any field the
// spec schema does not name, so a typo'd key fails loudly here instead of
// silently doing nothing. [LAW:single-enforcer] [LAW:no-silent-failure]
func ParseBulkSpecs(data []byte) ([]BulkIssueSpec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var specs []BulkIssueSpec
	for {
		var spec BulkIssueSpec
		if err := dec.Decode(&spec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("bulk: parse spec: %w", err)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// ParseImportTreeSpecs is the deserialization trust boundary for tree-import
// files: raw bytes in, specs out. It rejects any field the spec schema does
// not name and any trailing data after the array, so a drifted or typo'd spec
// fails loudly here instead of silently losing the unrecognized data downstream.
//
// [LAW:no-silent-failure] DisallowUnknownFields + trailing-data check make
// the parse total: every byte stream that is not exactly one array of
// known-field specs is an explicit error.
func ParseImportTreeSpecs(data []byte) ([]ImportTreeSpec, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var specs []ImportTreeSpec
	if err := dec.Decode(&specs); err != nil {
		return nil, fmt.Errorf("import: parse spec: %w", err)
	}
	if dec.More() {
		return nil, errors.New("import: unexpected trailing data after spec array")
	}
	return specs, nil
}
