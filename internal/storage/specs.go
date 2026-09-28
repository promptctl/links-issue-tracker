package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

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
// known-field specs is an explicit error. Each is a ValidationError because
// the file, not the moment, is what is wrong: rereading it unchanged can
// never parse.
func ParseImportTreeSpecs(data []byte) ([]ImportTreeSpec, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var specs []ImportTreeSpec
	if err := dec.Decode(&specs); err != nil {
		return nil, fmt.Errorf("import: parse spec: %w", treeSpecRefusal(err))
	}
	if dec.More() {
		return nil, ValidationError{Message: "import: unexpected trailing data after spec array"}
	}
	return specs, nil
}

// treeSpecRefusal names what the file is when its top-level value is not an
// array. The decoder would name the Go type it was filling instead, which
// tells the reader nothing about their file. The pointer to backup restore is
// there because the one JSON object a lit user is likely to hold is an
// export, and the export and the tree spec are two separate formats.
func treeSpecRefusal(err error) ValidationError {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Type == reflect.TypeFor[[]ImportTreeSpec]() {
		return ValidationError{Message: fmt.Sprintf("the file is a JSON %s, but a tree spec is a JSON array of records (see `lit import --help`). A lit export, as written by `lit export`, `lit backup create` or sync, is a JSON object `lit import` cannot read: load it with `lit backup restore --path <file>`", typeErr.Value)}
	}
	return ValidationError{Message: err.Error()}
}
