package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// This file is the wire form of ShapeMapping: the JSON an operator (the ambient
// LLM at the recovery boundary) authors and hands to `lit lifeboat recover
// --mapping`. The in-memory ShapeMapping is keyed by a struct (ColumnRef) and
// valued by a sealed interface (Disposition) — neither of which encoding/json
// can carry directly — so the wire form is a flat, ordered list of entries with
// an explicit kind discriminator.
//
// [LAW:one-source-of-truth] These are Marshal/Unmarshal METHODS on ShapeMapping,
// not a parallel DTO the rest of the package also knows: the wire shape is a
// projection of the one type, and decoding produces exactly that type. A second
// public struct mirroring ShapeMapping could drift from it; a method cannot.
//
// [LAW:single-enforcer] Decoding does NOT re-validate semantics. Totality,
// target resolution, drop-provenance validity, and table-shape are Validate's
// job — the one trust boundary every producer (deterministic, operator, future
// LLM) already passes before Apply. Decoding enforces only what Validate cannot
// see once the bytes have become a map: the kind discriminator that selects the
// sealed variant, and the absence of duplicate (table,column) keys that would
// silently overwrite one disposition with another during map assembly.

// dispositionKind is the closed wire discriminator for a Disposition. It names
// on the wire what the sealed interface names in memory, so "which variant"
// survives the round-trip explicitly rather than being inferred from which
// fields happen to be present.
type dispositionKind string

const (
	kindMapped  dispositionKind = "map"
	kindDropped dispositionKind = "drop"
)

// columnWire is one source column's disposition on the wire. Table and Column
// are the positional key; Kind selects which of the remaining fields are
// meaningful (To for a map; Provenance/Reason for a drop).
type columnWire struct {
	Table      string          `json:"table"`
	Column     string          `json:"column"`
	Kind       dispositionKind `json:"kind"`
	To         TargetKey       `json:"to,omitempty"`
	Provenance DropProvenance  `json:"provenance,omitempty"`
	Reason     string          `json:"reason,omitempty"`
}

type mappingWire struct {
	Columns []columnWire `json:"columns"`
}

// MarshalJSON renders the mapping as an ordered list so the artifact is stable
// across runs (sorted by table then column) — diffable, and reproducible when
// `lit lifeboat dump`-derived mappings are checked in or compared.
func (m ShapeMapping) MarshalJSON() ([]byte, error) {
	wire := mappingWire{Columns: make([]columnWire, 0, len(m.Columns))}
	for ref, disp := range m.Columns {
		entry := columnWire{Table: ref.Table, Column: ref.Column}
		switch d := disp.(type) {
		case MappedTo:
			entry.Kind = kindMapped
			entry.To = d.Target
		case Dropped:
			entry.Kind = kindDropped
			entry.Provenance = d.Provenance
			entry.Reason = d.Reason
		default:
			// [LAW:types-are-the-program] Disposition is sealed in this package,
			// so this arm is unreachable for any value the type system admits;
			// it errors rather than emitting a kindless entry that would decode
			// back to nothing.
			return nil, fmt.Errorf("shapemapping: column %s has unencodable disposition %T", ref, disp)
		}
		wire.Columns = append(wire.Columns, entry)
	}
	sort.Slice(wire.Columns, func(i, j int) bool {
		if wire.Columns[i].Table != wire.Columns[j].Table {
			return wire.Columns[i].Table < wire.Columns[j].Table
		}
		return wire.Columns[i].Column < wire.Columns[j].Column
	})
	return json.Marshal(wire)
}

// UnmarshalJSON builds the in-memory mapping from the wire list, rejecting the
// two malformations the downstream Validate cannot detect: an unrecognized kind
// (no sealed variant to produce) and a repeated (table,column) entry (which
// would silently collapse to one disposition).
func (m *ShapeMapping) UnmarshalJSON(data []byte) error {
	// [LAW:no-silent-fallbacks] The mapping file is an operator's authored
	// artifact at a trust boundary: an unknown field is a typo (e.g. "prov" for
	// "provenance"), not data to discard. Reject it here, where the byte that is
	// wrong is still in hand, rather than let json silently drop it and surface a
	// confusing "unaccounted-for column" three steps downstream in Validate.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wire mappingWire
	if err := dec.Decode(&wire); err != nil {
		return err
	}
	// A mapping file is one JSON document. Trailing non-whitespace (e.g. a second
	// object) means the artifact is malformed — the operator concatenated or
	// edited it wrong — and accepting only the first object would silently ignore
	// the rest. Require the stream to be exhausted.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return fmt.Errorf("shapemapping: unexpected trailing data after the mapping document")
	}
	cols := make(map[ColumnRef]Disposition, len(wire.Columns))
	for _, entry := range wire.Columns {
		ref := ColumnRef{Table: entry.Table, Column: entry.Column}
		if _, dup := cols[ref]; dup {
			return fmt.Errorf("shapemapping: duplicate disposition for column %s", ref)
		}
		switch entry.Kind {
		case kindMapped:
			cols[ref] = MappedTo{Target: entry.To}
		case kindDropped:
			cols[ref] = Dropped{Provenance: entry.Provenance, Reason: entry.Reason}
		default:
			return fmt.Errorf("shapemapping: column %s has unknown kind %q (want %q or %q)",
				ref, entry.Kind, kindMapped, kindDropped)
		}
	}
	m.Columns = cols
	return nil
}
