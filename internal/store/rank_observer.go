package store

import "context"

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

// RankObserver is told about the rank keys a write that stands wrote: the
// longest key of each write, and each respace. The store reports these as
// values and never records them itself, as with LockObserver.
// [LAW:effects-at-boundaries]
type RankObserver struct {
	Wrote    func(longestKey int)
	Respaced func(Respace)
}

type rankObserverKey struct{}

// WithRankObserver returns ctx carrying observer. Both funcs must be set: a nil
// one panics here, where it was installed, rather than inside a write.
func WithRankObserver(ctx context.Context, observer RankObserver) context.Context {
	if observer.Wrote == nil || observer.Respaced == nil {
		panic("store.WithRankObserver: both Wrote and Respaced must be set")
	}
	return context.WithValue(ctx, rankObserverKey{}, observer)
}

// rankObserverFrom is the observer ctx carries. Work run outside an invocation
// carries none and is reported to nobody.
func rankObserverFrom(ctx context.Context) RankObserver {
	if observer, ok := ctx.Value(rankObserverKey{}).(RankObserver); ok {
		return observer
	}
	return RankObserver{Wrote: func(int) {}, Respaced: func(Respace) {}}
}

func longestKey(keys []string) int {
	longest := 0
	for _, key := range keys {
		longest = max(longest, len(key))
	}
	return longest
}
