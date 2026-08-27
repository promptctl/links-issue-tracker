package storage

import "time"

// This file is the vocabulary the checkpoint and repair capabilities speak,
// relocated from the Dolt engine for the same reason the sync vocabulary was:
// an interface every engine can implement cannot name a type only one engine
// can spell. [LAW:one-source-of-truth]

// Checkpoint is a named revert point: a labelled anchor to the store's whole
// state at one instant, which the engine that made it can return the store to.
// Not migration-specific — any operation that needs a rollback anchor takes a
// different prefix and reuses this primitive.
//
// [LAW:types-are-the-program] The type encodes the full description of a revert
// point: name, prefix, creation time, and the engine-side identity of the state
// captured. The name encodes the prefix and timestamp so ListCheckpoints can
// reconstruct the set without external metadata storage.
type Checkpoint struct {
	Name      string    // "<prefix>-<unix-nano>"
	Prefix    string    // caller label, e.g. "pre-migrate"
	CreatedAt time.Time // parsed from the unix-nano suffix in Name
	// CommitSHA identifies the captured state to the engine that captured it.
	// It is opaque to every caller: the contract requires only that handing it
	// back names the same state, never that it is a hash or that it is a commit.
	CommitSHA string
}

// HealthReport is what an engine found when it examined itself: the structural
// faults it can name, plus free-text errors and warnings for what it can only
// describe. An engine reports zeros for the checks it has no analogue for
// rather than omitting them — an absent check and a clean check are different
// facts, and the capability's presence already says which checks it runs.
type HealthReport struct {
	IntegrityCheck     string   `json:"integrity_check"`
	ForeignKeyIssues   int      `json:"foreign_key_issues"`
	InvalidRelatedRows int      `json:"invalid_related_rows"`
	OrphanHistoryRows  int      `json:"orphan_history_rows"`
	RankInversions     int      `json:"rank_inversions"`
	DependencyCycle    []string `json:"dependency_cycle"`
	Errors             []string `json:"errors"`
	Warnings           []string `json:"warnings"`
}
