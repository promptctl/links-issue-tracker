package storage

// Landing is what a write that lands an export whole — a sync, a restore — had
// to change on the way in so the store's rules hold over what it landed: the
// parent edges it cut because they closed a loop, the tickets it moved into an
// epic, and the tickets it gave a rank key of their own. All are empty when the
// export already kept the rules.
type Landing struct {
	Unlooped []Unlooped `json:"unlooped,omitempty"`
	Rehomed  []Rehomed  `json:"rehomed,omitempty"`
	Rekeyed  []Rekeyed  `json:"rekeyed,omitempty"`
}

// Replacement is what a restore's replace did: the tickets it removed, and
// what landing the export under the store's rules changed. Both are read inside
// the replace's own transaction, so they describe the rows it cleared, not a
// read taken before it.
type Replacement struct {
	Removed []Removed
	Landing Landing
}

// Removed is a ticket the store held that the export, as it landed, does not.
// A ticket is its id and its birth (created_at), so one the export holds under
// the same id but born at another instant is removed too.
type Removed struct {
	IssueID string `json:"issue_id"`
	Title   string `json:"title"`
}

// Rekeyed is one ticket a landing gave a new rank key so that no two tickets
// share one: the key it held, and the key it holds now. Its place in every
// listing is unchanged: only the tie that the id used to break is gone.
type Rekeyed struct {
	IssueID string `json:"issue_id"`
	From    string `json:"from"`
	Rank    string `json:"rank"`
}
