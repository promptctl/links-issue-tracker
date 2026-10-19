package store

import (
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
// — under the three rules a single write is refused for breaking, and says what
// it changed: the hierarchy holds no loop (storage.BreakParentCycles), every
// ticket lives inside an epic (storage.HomeStranded), and every rank key orders
// one issue (separateRankCollisions). Cutting runs first because a leaf whose
// loop edge it cuts can be left with no epic, which homing then gives it;
// homing runs before the separation because it can mint the unfiled epic, and
// the epic's key must be one the separation sees.
//
// [LAW:single-enforcer] Every write that lands an export whole goes through
// here; the recovery rebuild alone does not, because it must reproduce its
// source.
func landExport(export model.Export) (model.Export, storage.Landing, error) {
	unlooped, cut := storage.BreakParentCycles(export)
	contained, rehomed := storage.HomeStranded(unlooped)
	separated, rekeyed, err := separateRankCollisions(contained)
	if err != nil {
		return model.Export{}, storage.Landing{}, err
	}
	return separated, storage.Landing{Unlooped: cut, Rehomed: rehomed, Rekeyed: rekeyed}, nil
}

// placedIssues returns the ranked issues of export that a rank verb places
// among: every one not deleted, the set Doctor counts too (loadRankOrder). A
// deleted issue lists nowhere, so the verbs' neighbour lookups step past it
// (`deleted_at IS NULL`, ranking.go) and can mint its key again; its key
// orders nothing.
func placedIssues(export model.Export) []rankedIssue {
	placed := make([]rankedIssue, 0, len(export.Issues))
	for _, issue := range export.Issues {
		if _, deleted := issue.Retention().(model.Deleted); deleted || issue.Rank == "" {
			continue
		}
		placed = append(placed, rankedIssue{id: issue.ID, rank: issue.Rank})
	}
	return placed
}

// separateRankCollisions returns export with every rank key its placed issues
// hold held by one of them alone, and names each issue it gave a new key, in
// listed order. An export with no shared key comes back unchanged with nothing
// named.
//
// Two issues reach one key at the boundaries no rank verb guards: a merge
// resolves each row's rank on its own, so replicas that each swapped a
// different pair can merge two rows onto one key, and a restore writes the keys
// its export holds. The order they list in now — key, then id — is the order
// the separation realizes, by the repair `lit doctor --fix=rank` makes
// (rankRewrites): the longest run of keys that already ascend strictly stays,
// so one holder of each shared key keeps it, and the rest are spaced into the
// gaps that run leaves. So every listing keeps its order, and no key the
// export holds — one with no room beside it, or a malformed one — leaves the
// landing without a gap to fill.
//
// It is pure, so two clones landing the same export write the same rows.
// [LAW:effects-at-boundaries]
func separateRankCollisions(export model.Export) (model.Export, []storage.Rekeyed, error) {
	placed := placedIssues(export)
	collisions := rankCollisions(placed)
	if len(collisions) == 0 {
		return export, nil, nil
	}
	slices.SortFunc(placed, compareRankedIssues)
	from := make(map[string]string, len(placed))
	for _, issue := range placed {
		from[issue.id] = issue.rank
	}
	rewrites, err := rankRewrites(anchorRun(placed))
	if err != nil {
		return model.Export{}, nil, fmt.Errorf("separate shared rank keys (%s): %w", describeRankCollisions(collisions), err)
	}

	newRank := make(map[string]string, len(rewrites))
	rekeyed := make([]storage.Rekeyed, len(rewrites))
	for i, rewrite := range rewrites {
		newRank[rewrite.id] = rewrite.newRank
		rekeyed[i] = storage.Rekeyed{IssueID: rewrite.id, From: from[rewrite.id], Rank: rewrite.newRank}
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
