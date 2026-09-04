package storage

// BulkIssueSpec is a single YAML document in a bulk-input file. ID is the
// selector: present, it names an existing issue and the document is an
// update patch of only the fields it sets (the same fields UpdateIssueInput
// exposes, one YAML key per field). Absent, the document creates a new
// issue and behaves like ImportTreeSpec's flat form — LocalID, Parent, and
// DependsOn wire the new issue against siblings created in the same file (by
// LocalID) or against pre-existing issues (by real ID); title/topic/type are
// required as they are for any create. [LAW:types-are-the-program] Pointer
// fields are exactly the update-patch fields: nil means "leave unchanged,"
// a set pointer means "write this value" — the same distinction
// UpdateIssueInput's pointers carry, so a create-doc's omitted optional
// field and an update-doc's omitted field defer to the same "unspecified"
// representation instead of each needing its own convention.
//
// The struct tags are contract, not engine detail: the file format is what a
// user authored and what any engine must keep reading identically.
type BulkIssueSpec struct {
	LocalID     string    `yaml:"local_id,omitempty"`
	ID          string    `yaml:"id,omitempty"`
	Title       *string   `yaml:"title,omitempty"`
	Description *string   `yaml:"description,omitempty"`
	Prompt      *string   `yaml:"prompt,omitempty"`
	IssueType   *string   `yaml:"type,omitempty"`
	Topic       *string   `yaml:"topic,omitempty"`
	Priority    *int      `yaml:"priority,omitempty"`
	Assignee    *string   `yaml:"assignee,omitempty"`
	Labels      *[]string `yaml:"labels,omitempty"`
	Lane        *string   `yaml:"lane,omitempty"`
	Parent      string    `yaml:"parent,omitempty"`
	DependsOn   []string  `yaml:"depends_on,omitempty"`
	// Reason is optional free text recorded on an update's field-change
	// event; it does not apply to a create (there is no prior state to
	// annotate a change against).
	Reason string `yaml:"reason,omitempty"`
}

// IDMapping records one created issue: Ref is the name the input file used
// for it, ID is the real issue ID it was created under.
//
// [LAW:types-are-the-program] A result carries these in the engine's creation
// order — an ordered slice, not a map — so the same input file reports the
// same sequence on every run instead of Go's randomized map iteration.
type IDMapping struct {
	Ref string
	ID  string
}

// NewIDMapping applies the one ref rule: a create is nameable by the LocalID
// it chose, or by its own new real ID when the file gave it none — so every
// create is in the report either way. [LAW:one-source-of-truth]
func NewIDMapping(localID, realID string) IDMapping {
	if localID == "" {
		return IDMapping{Ref: realID, ID: realID}
	}
	return IDMapping{Ref: localID, ID: realID}
}

// BulkApplyResult reports what a successful BulkApply did. Created lists
// every created issue in the order it was created; Updated lists the real IDs
// of every updated issue, in the order they were applied.
type BulkApplyResult struct {
	Created []IDMapping
	Updated []string
}

// ImportTreeSpec is a single record in a declarative tree-import file. LocalID
// is opaque — it's used inside the spec to wire Parent and DependsOn refs and
// is replaced with the generated lit issue ID at import time.
type ImportTreeSpec struct {
	LocalID     string   `json:"local_id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Prompt      string   `json:"prompt,omitempty"`
	IssueType   string   `json:"type"`
	Topic       string   `json:"topic"`
	Priority    int      `json:"priority"`
	Assignee    string   `json:"assignee,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Parent      string   `json:"parent,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

// ImportTreeResult reports the local-ID → real-issue-ID mapping produced by a
// successful import, in the order the issues were created.
type ImportTreeResult struct {
	Created []IDMapping
}
