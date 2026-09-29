package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/issueid"
	"github.com/promptctl/links-issue-tracker/internal/storage"
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
}

type idPrefixCount struct {
	prefix string
	issues int
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
// `unreadable:N` last when any id could not be read, and `none` for a store
// with no issues — a clean answer, not a missing one.
func (c idPrefixCensus) String() string {
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
func (c idPrefixCensus) mismatch(prefix string) bool {
	if len(c.counts) == 0 {
		return false
	}
	for _, count := range c.counts {
		if count.prefix == prefix {
			return false
		}
	}
	return true
}
