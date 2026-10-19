package main

import (
	"slices"
	"time"

	"github.com/promptctl/links-issue-tracker/internal/release"
)

// The support window: a schema this tree would publish must stay operable by
// the newest windowNewest releases AND by every release published within
// windowAge of now, whichever names more.
const (
	windowNewest = 2
	windowAge    = 30 * 24 * time.Hour
)

// published is one release and the moment it was published.
type published struct {
	tag release.Tag
	at  time.Time
}

// window is the releases a schema must stay operable by, newest first.
// [LAW:effects-at-boundaries] pure: the release list and the clock are inputs.
func window(releases []published, now time.Time) []published {
	newestFirst := slices.SortedFunc(slices.Values(releases), func(a, b published) int {
		return b.at.Compare(a.at)
	})
	cutoff := now.Add(-windowAge)
	end := 0
	for end < len(newestFirst) && (end < windowNewest || !newestFirst[end].at.Before(cutoff)) {
		end++
	}
	return slices.Clip(newestFirst[:end])
}
