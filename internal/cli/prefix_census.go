package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/engine"
	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/storage"
	"github.com/promptctl/links-issue-tracker/internal/store"
	"github.com/promptctl/links-issue-tracker/internal/workspace"
)

// idPrefixCensus is what the issue ids in a store actually use, read off the
// ids themselves. config.json names the prefix the NEXT issue is minted under;
// this is the evidence of every prefix issues were ever minted under, which is
// what tells a repair of config.json apart from a rewrite of it — and a stored
// prefix that disagrees with the backlog it sits over, however it got there.
// [LAW:one-source-of-truth] config.json is the value under suspicion, so the
// census never consults it.
type idPrefixCensus struct {
	// counts is ordered by issue count descending, ties by prefix, so the
	// prefix the backlog mostly uses reads first.
	counts []idPrefixCount
	// unreadable counts ids whose prefix cannot be read back: shapes minted by
	// no current rule, or a child whose root is gone. Counted rather than
	// dropped, so the total always matches the store. [LAW:no-silent-failure]
	unreadable int
	// noStore marks a workspace whose store `lit init` has not created yet:
	// there are no ids, which is not the same fact as a store holding none.
	// [LAW:nothing-unseen] zero and absent render differently.
	noStore bool
}

type idPrefixCount struct {
	prefix string
	issues int
}

// readWorkspaceIDPrefixCensus is the census `lit prefix set` takes. It opens
// the store itself, read-only, because the command must also run before the
// store exists: config.json can carry a refused prefix in a workspace `lit
// init` never finished, and init refuses on that prefix before it creates the
// store, so requiring one here would send each command's remediation to the
// other. Not-initialized is read as the census of an absent store; every other
// open failure propagates. [LAW:no-silent-failure]
func readWorkspaceIDPrefixCensus(ctx context.Context, ws workspace.Info) (census idPrefixCensus, err error) {
	st, err := engine.Open(ctx, engine.ReadOnly, ws.DatabasePath, ws.WorkspaceID)
	if errors.Is(err, store.ErrWorkspaceNotInitialized) {
		return idPrefixCensus{noStore: true}, nil
	}
	if err != nil {
		return idPrefixCensus{}, err
	}
	// Close releases the workspace's shared lock, so its error is surfaced, but
	// never over a read error. [LAW:no-silent-failure]
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	return readIDPrefixCensus(ctx, st)
}

// readIDPrefixCensus counts every issue the store holds — archived and deleted
// included, since their ids still occupy the prefix — by the prefix its id
// carries. It reads identities, not issues: `lit doctor` takes the census
// before anything has shown the hierarchy is free of loops, and hydrating an
// issue walks the hierarchy.
func readIDPrefixCensus(ctx context.Context, st storage.IssueReader) (idPrefixCensus, error) {
	issues, err := st.ListIssueIdentities(ctx)
	if err != nil {
		return idPrefixCensus{}, fmt.Errorf("read issue ids for the prefix census: %w", err)
	}
	return censusOf(issues), nil
}

// censusOf is the pure half of readIDPrefixCensus. A child id carries its
// root's prefix, and its own topic is not the one the root's id was rendered
// from, so every id is read through its root.
func censusOf(issues []storage.IssueIdentity) idPrefixCensus {
	topicOf := make(map[string]string, len(issues))
	for _, issue := range issues {
		topicOf[issue.ID] = issue.Topic
	}
	byPrefix := map[string]int{}
	var census idPrefixCensus
	for _, issue := range issues {
		root, _, _ := strings.Cut(issue.ID, ".")
		topic, known := topicOf[root]
		prefix, ok := issueid.PrefixOf(root, topic)
		if !known || !ok {
			census.unreadable++
			continue
		}
		byPrefix[prefix]++
	}
	for prefix, n := range byPrefix {
		census.counts = append(census.counts, idPrefixCount{prefix: prefix, issues: n})
	}
	sort.Slice(census.counts, func(i, j int) bool {
		if census.counts[i].issues != census.counts[j].issues {
			return census.counts[i].issues > census.counts[j].issues
		}
		return census.counts[i].prefix < census.counts[j].prefix
	})
	return census
}

// String renders the census as one field value: `links:118,demo:2`, with
// `unreadable:N` last when any id could not be read, `none` for a store with no
// issues — a clean answer, not a missing one — and `none (no store yet)` before
// the store exists.
func (c idPrefixCensus) String() string {
	if c.noStore {
		return "none (no store yet)"
	}
	parts := make([]string, 0, len(c.counts)+1)
	for _, count := range c.counts {
		parts = append(parts, fmt.Sprintf("%s:%d", count.prefix, count.issues))
	}
	if c.unreadable > 0 {
		parts = append(parts, fmt.Sprintf("unreadable:%d", c.unreadable))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

// mismatch reports whether the store holds readable ids and none of them
// carries prefix: the next issue would be minted under a prefix no existing
// issue uses. That is legitimate straight after a deliberate `lit prefix set`,
// and it is also what adopting a backlog minted under another prefix looks
// like, so it is reported, never refused.
func (c idPrefixCensus) mismatch(prefix workspace.StoredPrefix) bool {
	if len(c.counts) == 0 {
		return false
	}
	for _, count := range c.counts {
		if workspace.StoredPrefix(count.prefix) == prefix {
			return false
		}
	}
	return true
}

// adoptable is the prefix a mismatch report suggests: the most-used one that
// `lit prefix set` would store exactly as the ids carry it. Ids minted under a
// prefix the current rules refuse cannot be adopted, so when no counted prefix
// qualifies the suggestion is the placeholder — naming a command that fails is
// the false remediation the error mapping exists to prevent.
// [LAW:single-enforcer] legality is workspace.ConfiguredPrefix's answer, the
// same boundary `lit prefix set` parses its argument through.
func (c idPrefixCensus) adoptable() string {
	for _, count := range c.counts {
		if spec, err := workspace.ConfiguredPrefix(count.prefix); err == nil && spec.Value() == count.prefix {
			return count.prefix
		}
	}
	return "<prefix>"
}
