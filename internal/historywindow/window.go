// Package historywindow is the window the sync branch's history is kept
// inside: the one value both the config that names it and the store that
// keeps it speak.
package historywindow

import "fmt"

// Window is the bound on the sync branch's reachable history, as a
// commit count: a push that leaves the remote head at height ReRootAt or
// above re-roots it, leaving the head at a height from Keep up to ReRootAt.
// It is a count, not an age, because the store grows linearly with its
// commits while the rate of commits swings month to month (the decision on
// links-storage-3k6u). A re-root always keeps the head and always drops
// something, so New builds only a window that keeps at least one commit and
// fewer than it re-roots at. [LAW:types-are-the-program] The zero value is the
// one Window New never builds, so it is no window: reading it is a defect,
// and panics as one, rather than bounding a shared remote's history by
// whatever its zero fields would say. [LAW:no-silent-failure]
type Window struct {
	reRootAt, keep uint64
}

// New is the window that re-roots at reRootAt and keeps keep, refused unless
// 1 <= keep < reRootAt.
func New(reRootAt, keep uint64) (Window, error) {
	window := Window{reRootAt: reRootAt, keep: keep}
	if keep < 1 || keep >= reRootAt {
		return Window{}, fmt.Errorf("a history window keeps at least one commit and fewer than it re-roots at; %s is not one", window.spelled())
	}
	return window, nil
}

// built is w, once New built it.
func (w Window) built() Window {
	if w.keep == 0 {
		panic("historywindow: the zero Window read; every window is built by New")
	}
	return w
}

// ReRootAt is the remote head height at which a push re-roots.
func (w Window) ReRootAt() uint64 { return w.built().reRootAt }

// Keep is the least height a re-root leaves the head at: it cuts at the
// highest commit every line passes through that keeps that many, and below
// ReRootAt.
func (w Window) Keep() uint64 { return w.built().keep }

// String is the window as the config spells it.
func (w Window) String() string { return w.built().spelled() }

func (w Window) spelled() string {
	return fmt.Sprintf("re_root_at %d, keep %d", w.reRootAt, w.keep)
}
