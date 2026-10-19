package storage

// BulkIssueSpec is a single YAML document in a bulk-input file. ID is the
// selector: present, it names an existing issue and the document is an
// update patch of only the fields it sets (the same fields UpdateIssueInput
// exposes, one YAML key per field). Absent, the document creates a new
// issue and behaves like ImportTreeSpec's flat form — LocalID, Parent, and
// BlockedBy wire the new issue against siblings created in the same file (by
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
	Plan        *string   `yaml:"plan,omitempty"`
	IssueType   *string   `yaml:"type,omitempty"`
	Topic       *string   `yaml:"topic,omitempty"`
	Priority    *int      `yaml:"priority,omitempty"`
	Assignee    *string   `yaml:"assignee,omitempty"`
	Labels      *[]string `yaml:"labels,omitempty"`
	Lane        *string   `yaml:"lane,omitempty"`
	Parent      string    `yaml:"parent,omitempty"`
	BlockedBy   []string  `yaml:"blocked_by,omitempty"`
	// Rank places the document's issue in the file's ordering. Its one
	// value is "top"; the gate refuses any other.
	Rank *string `yaml:"rank,omitempty"`
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
// of every updated issue, in the order they were applied; Ranked is the
// RankSet the file's `rank: top` documents made, with no resolutions when
// the file states no rank.
type BulkApplyResult struct {
	Created []IDMapping
	Updated []string
	Ranked  RankSetResult
}

// BulkFile is a bulk file the gate admitted, parsed: the issues it creates,
// in the order they are written, and the patch each update document applies.
// An engine writes exactly this, so a document means the same write whichever
// engine opens the file, and no engine parses a document itself.
// [LAW:parse-dont-validate] [LAW:one-source-of-truth]
//
// Ranked is the file's ordering: every document that states `rank: top`, in
// document order. An engine hands it to its RankSet whole, so a file orders
// issues exactly as `lit rank set` does — one transaction, keys permuted and
// none minted, and an order that has already landed writes nothing.
// [LAW:single-enforcer]
type BulkFile struct {
	Creates []FileCreate
	Updates []BulkUpdate
	Ranked  []FileRef
}

// FileRef names a document's issue before the file is written: an update
// document's id, or a create document's position in the file, whose id only
// the write mints.
type FileRef struct {
	id    string
	index int
}

// RankedIDs is the file's ordering as the issue ids an engine hands its
// RankSet, given the id the write minted for each create document by its
// position. It is empty for a file that states no rank.
func (f BulkFile) RankedIDs(createdID func(index int) string) []string {
	ids := make([]string, len(f.Ranked))
	for i, ref := range f.Ranked {
		ids[i] = ref.id
		if ref.id == "" {
			ids[i] = createdID(ref.index)
		}
	}
	return ids
}

// BulkUpdate is one update document, parsed: the issue it names and the patch
// of the fields it states.
type BulkUpdate struct {
	ID    string
	Patch UpdateIssueInput
}

// FileCreate is one create document of an import file, parsed. Index is its
// position in the file, which is how a bulk refusal names it; Parent and
// BlockedBy are references, each a local_id in the file or a real id, which
// only the write can resolve.
type FileCreate struct {
	Index     int
	LocalID   string
	Parent    string
	BlockedBy []string
	issue     CreateIssueInput
}

// Input is the create this document writes, given the three facts only the
// write knows: the real id its parent reference resolved to, the prefix of the
// workspace it lands in, and who is importing it. Taking them as arguments is
// what keeps a write from landing with its parent unresolved or its author
// unnamed. [LAW:types-are-the-program]
func (c FileCreate) Input(parentID, prefix, actor string) CreateIssueInput {
	in := c.issue
	in.ParentID = parentID
	in.Prefix = prefix
	in.Actor = actor
	return in
}

// ImportTreeSpec is a single record in a declarative tree-import file. LocalID
// is opaque — it's used inside the spec to wire Parent and BlockedBy refs and
// is replaced with the generated lit issue ID at import time.
type ImportTreeSpec struct {
	LocalID     string   `json:"local_id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Prompt      string   `json:"prompt,omitempty"`
	Plan        string   `json:"plan,omitempty"`
	IssueType   string   `json:"type"`
	Topic       string   `json:"topic"`
	Priority    int      `json:"priority"`
	Assignee    string   `json:"assignee,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Parent      string   `json:"parent,omitempty"`
	BlockedBy   []string `json:"blocked_by,omitempty"`
}

// ImportTreeResult reports the local-ID → real-issue-ID mapping produced by a
// successful import, in the order the issues were created.
type ImportTreeResult struct {
	Created []IDMapping
}
