package store

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// rankCollision is one rank key more than one issue holds, and its holders in
// the order they list in while they share it: by id, the tiebreak that decides
// their order in place of anything a caller ranked.
type rankCollision struct {
	key string
	ids []string
}

// rankCollisions returns every key two or more of issues hold, in key order.
// An empty rank is an unranked issue rather than a key, so it collides with
// nothing.
//
// [LAW:single-enforcer] This is the one definition of a duplicate rank: Doctor
// counts what it returns, a landing separates it, and the recovery gate refuses
// a rebuild that holds it.
func rankCollisions(issues []rankedIssue) []rankCollision {
	holders := map[string][]string{}
	for _, issue := range issues {
		if issue.rank == "" {
			continue
		}
		holders[issue.rank] = append(holders[issue.rank], issue.id)
	}
	collisions := make([]rankCollision, 0)
	for key, ids := range holders {
		if len(ids) < 2 {
			continue
		}
		slices.Sort(ids)
		collisions = append(collisions, rankCollision{key: key, ids: ids})
	}
	slices.SortFunc(collisions, func(a, b rankCollision) int { return strings.Compare(a.key, b.key) })
	return collisions
}

// describeRankCollisions renders each collision as its key and its holders.
func describeRankCollisions(collisions []rankCollision) string {
	described := make([]string, len(collisions))
	for i, c := range collisions {
		described[i] = fmt.Sprintf("%q shared by %s", c.key, strings.Join(c.ids, ", "))
	}
	return strings.Join(described, "; ")
}

// landExport brings an export that lands whole — a restore, every sync landing
// — under the two rules a single write is refused for breaking, and says what it
// changed: every ticket lives inside an epic (storage.HomeStranded), and every
// rank key orders one issue (separateRankCollisions). Homing runs first because
// it can mint the unfiled epic, and the epic's key must be one the separation
// sees.
//
// [LAW:single-enforcer] Every write that lands an export whole goes through
// here; the recovery rebuild alone does not, because it must reproduce its
// source.
func landExport(export model.Export) (model.Export, storage.Landing, error) {
	contained, rehomed := storage.HomeStranded(export)
	separated, rekeyed, err := separateRankCollisions(contained)
	if err != nil {
		return model.Export{}, storage.Landing{}, err
	}
	return separated, storage.Landing{Rehomed: rehomed, Rekeyed: rekeyed}, nil
}

// separateRankCollisions returns export with every rank key held by one issue
// alone, and names each issue it gave a new key, in listed order. An export
// with no shared key comes back unchanged with nothing named.
//
// Two issues reach one key at the boundaries no rank verb guards: a merge
// resolves each row's rank on its own, so replicas that each swapped a
// different pair can merge two rows onto one key, and a restore writes the keys
// its export holds. Every holder of a shared key moves, and the issues whose key
// is their own stay where they are: the movers are spaced, in the order they
// list in now — key, then id — into the gap between the keys around them. So
// every listing keeps its order, and only the key that tied changes.
//
// It is pure, and every key it writes derives from the export, so two clones
// separating the same collision write the same rows. [LAW:effects-at-boundaries]
func separateRankCollisions(export model.Export) (model.Export, []storage.Rekeyed, error) {
	order := make([]rankedIssue, 0, len(export.Issues))
	for _, issue := range export.Issues {
		if issue.Rank == "" {
			continue
		}
		order = append(order, rankedIssue{id: issue.ID, rank: issue.Rank})
	}
	collisions := rankCollisions(order)
	if len(collisions) == 0 {
		return export, nil, nil
	}
	shared := make(map[string]bool, len(collisions))
	for _, c := range collisions {
		shared[c.key] = true
	}
	slices.SortFunc(order, func(a, b rankedIssue) int {
		return cmp.Or(strings.Compare(a.rank, b.rank), strings.Compare(a.id, b.id))
	})
	target := make([]string, len(order))
	rankOf := make(map[string]string, len(order))
	anchored := make([]bool, len(order))
	for i, issue := range order {
		target[i] = issue.id
		rankOf[issue.id] = issue.rank
		anchored[i] = !shared[issue.rank]
	}
	rewrites, err := spaceMovers(target, rankOf, anchored)
	if err != nil {
		return model.Export{}, nil, fmt.Errorf("separate shared rank keys (%s): %w", describeRankCollisions(collisions), err)
	}

	newRank := make(map[string]string, len(rewrites))
	rekeyed := make([]storage.Rekeyed, len(rewrites))
	for i, rewrite := range rewrites {
		newRank[rewrite.id] = rewrite.newRank
		rekeyed[i] = storage.Rekeyed{IssueID: rewrite.id, Shared: rankOf[rewrite.id], Rank: rewrite.newRank}
	}
	out := export
	out.Issues = slices.Clone(export.Issues)
	for i, issue := range out.Issues {
		if r, moved := newRank[issue.ID]; moved {
			out.Issues[i].Rank = r
		}
	}
	return out, rekeyed, nil
}
