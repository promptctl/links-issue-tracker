package storage

// Landing is what a write that lands an export whole — a sync, a restore — had
// to change on the way in so the store's rules hold over what it landed: the
// tickets it moved into an epic, and the tickets it gave a rank key of their
// own. Both are empty when the export already kept the rules.
type Landing struct {
	Rehomed []Rehomed `json:"rehomed,omitempty"`
	Rekeyed []Rekeyed `json:"rekeyed,omitempty"`
}

// Rekeyed is one ticket a landing gave a new rank key so that no two tickets
// share one: the key it held, and the key it holds now. Its place in every
// listing is unchanged: only the tie that the id used to break is gone.
type Rekeyed struct {
	IssueID string `json:"issue_id"`
	From    string `json:"from"`
	Rank    string `json:"rank"`
}
