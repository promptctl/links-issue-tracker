package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// This file is the wire form of ShapeMapping: the JSON an operator (the ambient
// LLM at the recovery boundary) authors and hands to `lit lifeboat recover
// --mapping`. The in-memory ShapeMapping nests sealed interfaces (FieldSource,
// EmitCondition) and maps that encoding/json cannot carry directly, so the wire
// form is an ordered, discriminated projection: tables → emitters → fields, plus
// each table's drops.
//
// [LAW:one-source-of-truth] These are Marshal/Unmarshal METHODS on ShapeMapping,
// not a parallel DTO the rest of the package also knows: the wire shape is a
// projection of the one type, and decoding produces exactly that type. A second
// public struct mirroring ShapeMapping could drift from it; a method cannot.
//
// [LAW:single-enforcer] Decoding does NOT re-validate semantics. Totality, target
// resolution, transform admissibility, collection validity, required coverage,
// and condition validity are Validate's job — the one trust boundary every
// producer already passes before Apply. Decoding enforces only what Validate
// cannot see once the bytes have become typed values: the kind discriminators
// that select the sealed variants, and the absence of duplicate keys (a table
// dispositioned twice, a field assigned twice within an emitter, a column dropped
// twice) that would silently collapse during the projection back into maps.

// fieldSourceKind is the closed wire discriminator for a FieldSource.
type fieldSourceKind string

const (
	sourceColumn fieldSourceKind = "column"
	sourceConst  fieldSourceKind = "const"
)

// whenKind is the closed wire discriminator for an EmitCondition.
type whenKind string

const (
	whenAlways  whenKind = "always"
	whenChanged whenKind = "changed"
)

type fieldWire struct {
	Field     string          `json:"field"`
	Source    fieldSourceKind `json:"source"`
	Column    string          `json:"column,omitempty"`
	Transform Transform       `json:"transform,omitempty"`
	Value     string          `json:"value,omitempty"`
}

type whenWire struct {
	Kind   whenKind `json:"kind"`
	FieldA string   `json:"fieldA,omitempty"`
	FieldB string   `json:"fieldB,omitempty"`
}

type emitterWire struct {
	Collection string      `json:"collection"`
	When       whenWire    `json:"when"`
	Fields     []fieldWire `json:"fields"`
}

type dropWire struct {
	Column     string         `json:"column"`
	Provenance DropProvenance `json:"provenance"`
	Reason     string         `json:"reason,omitempty"`
}

type tableWire struct {
	Table    string        `json:"table"`
	Emitters []emitterWire `json:"emitters,omitempty"`
	Drops    []dropWire    `json:"drops,omitempty"`
}

type mappingWire struct {
	Tables []tableWire `json:"tables"`
}

// MarshalJSON renders the mapping as ordered, sorted lists so the artifact is
// stable across runs — diffable, and reproducible when `lit lifeboat dump`-derived
// mappings are checked in or compared.
func (m ShapeMapping) MarshalJSON() ([]byte, error) {
	wire := mappingWire{Tables: make([]tableWire, 0, len(m.Tables))}
	for _, tm := range m.Tables {
		tw := tableWire{Table: tm.Table}
		for _, em := range tm.Emitters {
			ew, err := emitterToWire(tm.Table, em)
			if err != nil {
				return nil, err
			}
			tw.Emitters = append(tw.Emitters, ew)
		}
		for col, d := range tm.Drops {
			tw.Drops = append(tw.Drops, dropWire{Column: col, Provenance: d.Provenance, Reason: d.Reason})
		}
		sortEmitters(tw.Emitters)
		sort.Slice(tw.Drops, func(i, j int) bool { return tw.Drops[i].Column < tw.Drops[j].Column })
		wire.Tables = append(wire.Tables, tw)
	}
	sort.Slice(wire.Tables, func(i, j int) bool { return wire.Tables[i].Table < wire.Tables[j].Table })
	return json.Marshal(wire)
}

func emitterToWire(table string, em Emitter) (emitterWire, error) {
	ew := emitterWire{Collection: string(em.Collection)}
	switch w := em.When.(type) {
	case Always:
		ew.When = whenWire{Kind: whenAlways}
	case WhenChanged:
		ew.When = whenWire{Kind: whenChanged, FieldA: w.FieldA, FieldB: w.FieldB}
	default:
		// [LAW:types-are-the-program] EmitCondition is sealed in this package, so
		// this arm is unreachable for any value the type system admits; it errors
		// rather than emitting a kindless condition that would decode to nothing.
		return emitterWire{}, fmt.Errorf("shapemapping: table %q emitter has unencodable condition %T", table, em.When)
	}
	for field, src := range em.Fields {
		fw := fieldWire{Field: field}
		switch s := src.(type) {
		case FromColumn:
			fw.Source = sourceColumn
			fw.Column = s.Column
			fw.Transform = s.Transform
		case Constant:
			str, ok := s.Value.(string)
			if !ok {
				// The wire form carries string constants only — the value domain of
				// a dump is text, and Validate admits a constant only on a string
				// passthrough field. A non-string constant is unrepresentable here.
				return emitterWire{}, fmt.Errorf("shapemapping: table %q field %q constant must be a string, got %T", table, field, s.Value)
			}
			fw.Source = sourceConst
			fw.Value = str
		default:
			return emitterWire{}, fmt.Errorf("shapemapping: table %q field %q has unencodable source %T", table, field, src)
		}
		ew.Fields = append(ew.Fields, fw)
	}
	sort.Slice(ew.Fields, func(i, j int) bool { return ew.Fields[i].Field < ew.Fields[j].Field })
	return ew, nil
}

// sortEmitters orders a table's emitters by a TOTAL canonical key, so the wire
// form is reproducible even for a table that legitimately carries more than one
// emitter into the same collection. [LAW:one-source-of-truth] The key must
// distinguish every pair of emitters that differ in any encoded byte — collection,
// condition, and each field's full spec (name, source, column, transform, value).
// A key on field NAMES alone would tie two emitters that share field names but
// differ in source or condition, and sort.Slice (unstable) could then swap them,
// giving one mapping two encodings. Two emitters with equal keys are byte-identical,
// so their relative order is immaterial.
func sortEmitters(ems []emitterWire) {
	sort.Slice(ems, func(i, j int) bool {
		return emitterSortKey(ems[i]) < emitterSortKey(ems[j])
	})
}

// emitterSortKey serializes an emitter's full identity. Fields are already sorted
// by name (emitterToWire), so iterating them yields a stable byte sequence; NUL
// and SOH separators keep adjacent fields from aliasing (e.g. "ab"+"c" vs "a"+"bc").
func emitterSortKey(ew emitterWire) string {
	var b strings.Builder
	b.WriteString(ew.Collection)
	b.WriteByte(0)
	b.WriteString(string(ew.When.Kind))
	b.WriteByte(0)
	b.WriteString(ew.When.FieldA)
	b.WriteByte(0)
	b.WriteString(ew.When.FieldB)
	for _, f := range ew.Fields {
		b.WriteByte(1)
		b.WriteString(f.Field)
		b.WriteByte(0)
		b.WriteString(string(f.Source))
		b.WriteByte(0)
		b.WriteString(f.Column)
		b.WriteByte(0)
		b.WriteString(string(f.Transform))
		b.WriteByte(0)
		b.WriteString(f.Value)
	}
	return b.String()
}

// UnmarshalJSON builds the in-memory mapping from the wire form, rejecting the
// malformations the downstream Validate cannot detect: unknown discriminators (no
// sealed variant to produce) and repeated keys (which would silently collapse).
func (m *ShapeMapping) UnmarshalJSON(data []byte) error {
	// [LAW:no-silent-fallbacks] The mapping file is an operator's authored artifact
	// at a trust boundary: an unknown field is a typo, not data to discard. Reject
	// it here, where the byte that is wrong is still in hand, rather than let json
	// silently drop it and surface a confusing error several steps downstream.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire mappingWire
	if err := dec.Decode(&wire); err != nil {
		return err
	}
	// A mapping file is one JSON document. Trailing non-whitespace means the
	// artifact is malformed — the operator concatenated or edited it wrong — and
	// accepting only the first object would silently ignore the rest.
	switch trailing := dec.Decode(new(json.RawMessage)); trailing {
	case io.EOF:
	case nil:
		return fmt.Errorf("shapemapping: unexpected trailing data after the mapping document")
	default:
		return fmt.Errorf("shapemapping: malformed trailing data after the mapping document: %w", trailing)
	}

	tables := make([]TableMapping, 0, len(wire.Tables))
	seenTable := map[string]bool{}
	for _, tw := range wire.Tables {
		if seenTable[tw.Table] {
			return fmt.Errorf("shapemapping: duplicate disposition for table %q", tw.Table)
		}
		seenTable[tw.Table] = true
		tm, err := tableFromWire(tw)
		if err != nil {
			return err
		}
		tables = append(tables, tm)
	}
	m.Tables = tables
	return nil
}

func tableFromWire(tw tableWire) (TableMapping, error) {
	tm := TableMapping{Table: tw.Table}
	for _, ew := range tw.Emitters {
		em, err := emitterFromWire(tw.Table, ew)
		if err != nil {
			return TableMapping{}, err
		}
		tm.Emitters = append(tm.Emitters, em)
	}
	if len(tw.Drops) > 0 {
		tm.Drops = make(map[string]Dropped, len(tw.Drops))
		for _, dw := range tw.Drops {
			if _, dup := tm.Drops[dw.Column]; dup {
				return TableMapping{}, fmt.Errorf("shapemapping: table %q drops column %q more than once", tw.Table, dw.Column)
			}
			tm.Drops[dw.Column] = Dropped{Provenance: dw.Provenance, Reason: dw.Reason}
		}
	}
	return tm, nil
}

func emitterFromWire(table string, ew emitterWire) (Emitter, error) {
	em := Emitter{Collection: collection(ew.Collection)}
	switch ew.When.Kind {
	case whenAlways:
		em.When = Always{}
	case whenChanged:
		em.When = WhenChanged{FieldA: ew.When.FieldA, FieldB: ew.When.FieldB}
	default:
		return Emitter{}, fmt.Errorf("shapemapping: table %q emitter has unknown condition kind %q (want %q or %q)",
			table, ew.When.Kind, whenAlways, whenChanged)
	}
	em.Fields = make(map[string]FieldSource, len(ew.Fields))
	for _, fw := range ew.Fields {
		if _, dup := em.Fields[fw.Field]; dup {
			return Emitter{}, fmt.Errorf("shapemapping: table %q emitter into %q assigns field %q more than once", table, ew.Collection, fw.Field)
		}
		switch fw.Source {
		case sourceColumn:
			em.Fields[fw.Field] = FromColumn{Column: fw.Column, Transform: fw.Transform}
		case sourceConst:
			em.Fields[fw.Field] = Constant{Value: fw.Value}
		default:
			return Emitter{}, fmt.Errorf("shapemapping: table %q field %q has unknown source %q (want %q or %q)",
				table, fw.Field, fw.Source, sourceColumn, sourceConst)
		}
	}
	return em, nil
}
