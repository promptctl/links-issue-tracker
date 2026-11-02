package store

import (
	"context"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// Respace is one window a smoothing respace rewrote, and what it cost: the rows
// it moved and the length of the keys it left. A window past rank.MaxRespace
// had no room inside the cap or rows sharing the trigger's key, and keys at or
// past Ceiling are the density the cap left to key length.
type Respace struct {
	Rows       int
	Rewritten  int
	LongestKey int
	Ceiling    int
}

// RankSetAccount is what one rank set decided: the frame it wrote in, how many
// issues it was named, and the keys it wrote. Rewritten splits into Touched, a
// named issue that moved and is stamped, and Displaced, a key rewritten under
// an issue that stays where it stood. Minted is how many named issues took a
// new key past the frame's top rather than one handed round from the head. An
// order that had already arrived writes nothing and says 0 to each.
type RankSetAccount struct {
	Frame     storage.Frame
	Named     int
	Rewritten int
	Touched   int
	Displaced int
	Minted    int
}

// RankObserver is told about the rank keys a write that stands wrote: the
// longest key of each write, and each respace. It is told too what tree order
// cost a read: each query of parent edges, by the edges it returned, which is
// one a level for a climb and one for the whole read — and the shape of each
// ancestry built from them. And it is told what each rank set decided. The
// store reports these as
// values and never records them itself, as with LockObserver.
// [LAW:effects-at-boundaries]
type RankObserver struct {
	Wrote           func(longestKey int)
	Respaced        func(Respace)
	ReadParentEdges func(links int)
	Ancestry        func(storage.AncestryShape)
	RankSet         func(RankSetAccount)
}

type rankObserverKey struct{}

// WithRankObserver returns ctx carrying observer. Every func must be set: a nil
// one panics here, where it was installed, rather than inside a write.
func WithRankObserver(ctx context.Context, observer RankObserver) context.Context {
	if observer.Wrote == nil || observer.Respaced == nil || observer.ReadParentEdges == nil || observer.Ancestry == nil || observer.RankSet == nil {
		panic("store.WithRankObserver: Wrote, Respaced, ReadParentEdges, Ancestry and RankSet must all be set")
	}
	return context.WithValue(ctx, rankObserverKey{}, observer)
}

// rankObserverFrom is the observer ctx carries. Work run outside an invocation
// carries none and is reported to nobody.
func rankObserverFrom(ctx context.Context) RankObserver {
	if observer, ok := ctx.Value(rankObserverKey{}).(RankObserver); ok {
		return observer
	}
	return RankObserver{Wrote: func(int) {}, Respaced: func(Respace) {}, ReadParentEdges: func(int) {}, Ancestry: func(storage.AncestryShape) {}, RankSet: func(RankSetAccount) {}}
}

func longestKey(keys []string) int {
	longest := 0
	for _, key := range keys {
		longest = max(longest, len(key))
	}
	return longest
}
