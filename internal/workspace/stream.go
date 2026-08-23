package workspace

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// streamTokenFile is the write-once file holding a checkout's stream id, and it
// lives in the checkout's PRIVATE git directory — never the common one. That
// placement is the entire lifecycle design, and it is the deliberate mirror of
// deriveLocation: the store is derived from --git-common-dir so every worktree
// of a repository reaches ONE backlog, while identity is derived from --git-dir
// so every worktree carries a DISTINCT token. Git owns both facts, so identity
// needs no lifecycle of ours: it is created with the worktree, is never shared
// between worktrees, and is deleted by `git worktree remove` along with the
// directory containing it. [LAW:one-source-of-truth]
const streamTokenFile = "lit-stream"

// streamTokenBytes is the entropy behind a stream token. Eight bytes is far more
// than the domain needs — the population is the checkouts of one repository,
// tens at the very most — and it encodes to a 13-character token, which keeps
// the value short enough to sit on every work event in the synced database.
const streamTokenBytes = 8

// streamTokenLen is the encoded length implied by streamTokenBytes under
// unpadded base32 (ceil(8*8/5)). Derived from the byte count rather than written
// as its own constant, so the two cannot disagree if the entropy ever changes.
const streamTokenLen = (streamTokenBytes*8 + 4) / 5

// streamTokenEncoding renders token bytes as unpadded base32. The output is
// lowercased before use, giving the alphabet [a-z2-7]: no separators, no case
// significance, nothing that needs quoting in a shell, a URL, or a SQL literal.
var streamTokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// StreamID is a checkout's opaque stream token — the identity half of the
// attribution pair that work events carry. It is deliberately meaningless: no
// directory name, no hostname, no username material, because it travels into a
// database that syncs to shared remotes, where the design admits only opaque
// discriminators (design-docs/work-claims.md, "The privacy invariant").
//
// [LAW:types-are-the-program] The unexported field is what makes the type a
// claim rather than a comment: a StreamID cannot be conjured from an arbitrary
// string, so every value in the program came from minting or from parsing a
// token off disk, and no caller can smuggle a hostname into one.
//
// The zero value means "this checkout has never minted a token", which is a
// true and useful state rather than an error: a checkout that has never mutated
// has produced no evidence, and so can hold no claim.
type StreamID struct{ value string }

// Value returns the token as it is stored and stamped. Callers render work, not
// tokens — display resolves a token to local context on the machine that owns
// that context — so this exists for attribution and comparison, not for showing
// to anyone.
func (s StreamID) Value() string { return s.value }

// Present reports whether this checkout has minted a token yet. It distinguishes
// "no token, because nothing has ever mutated here" from a minted one; it is
// never a validity check, because an absent StreamID is the only invalid one a
// caller can hold. [LAW:parse-dont-validate]
func (s StreamID) Present() bool { return s.value != "" }

// ReadStream returns the stream id already minted in this checkout, minting
// NOTHING. It is the read-only half of the pair, and the reason read-only
// commands leave a never-mutated checkout untouched.
//
// A missing file and a corrupt one are deliberately not the same outcome. The
// file being absent is the ordinary "nothing has mutated here yet" state and
// yields the absent StreamID; a file that exists but does not hold a token is
// damage, and it surfaces as an error rather than being laundered into the same
// value as absence — those two facts must stay distinguishable, because
// collapsing them would report a checkout with a corrupted identity as a
// pristine one. [LAW:no-silent-failure]
func ReadStream(privateGitDir string) (StreamID, error) {
	path := filepath.Join(privateGitDir, streamTokenFile)
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return StreamID{}, nil
	}
	if err != nil {
		return StreamID{}, fmt.Errorf("read stream id %q: %w", path, err)
	}
	return parseStreamToken(path, string(payload))
}

// EnsureStream returns this checkout's stream id, minting one on first use. It
// is the write half of the pair: only a mutating command reaches it, which is
// what makes the token appear on a checkout's first mutation and not before.
//
// [LAW:dataflow-not-control-flow] The steps do not vary with whether a token
// already exists. A candidate is minted every time, the exclusive create is
// attempted every time, and the file is read back every time; only the VALUE
// differs — the candidate wins on a fresh checkout and is discarded on every
// later call. Structuring it as "check, then maybe mint" would both branch the
// mechanics and open the check-then-act race below.
//
// [LAW:single-enforcer] Write-once is enforced by the kernel through O_EXCL, not
// by this process reading before it writes. Two mutating commands starting
// together in one checkout would both observe "absent" under a read-first
// design and both write, and the second would silently replace an identity the
// first had already stamped onto events. With O_EXCL exactly one create can
// succeed; the loser is not an error case but the ordinary path on every call
// after the first, and it adopts the winner's token by reading it back.
func EnsureStream(privateGitDir string) (StreamID, error) {
	path := filepath.Join(privateGitDir, streamTokenFile)
	candidate, err := newStreamToken()
	if err != nil {
		return StreamID{}, err
	}
	if err := createStreamToken(path, candidate); err != nil {
		return StreamID{}, err
	}
	// The file, never this process, is the authority on what the token IS: the
	// candidate above is only a proposal, and reading back is what makes the
	// winner's value the one every caller sees. [LAW:one-source-of-truth]
	minted, err := ReadStream(privateGitDir)
	if err != nil {
		return StreamID{}, err
	}
	if !minted.Present() {
		// Reachable only if something removed the file between the create above
		// and this read. Returning the absent StreamID would hand a mutating
		// command an identity-less pair that reads exactly like a never-mutated
		// checkout, so the anomaly stops here instead. [LAW:no-silent-failure]
		return StreamID{}, fmt.Errorf("stream id %q vanished immediately after it was written", path)
	}
	return minted, nil
}

// createStreamToken writes a token only if the file does not yet exist. An
// already-existing file is a SUCCESSFUL outcome, not a failure — it is what
// every call after a checkout's first one looks like — so it is absorbed here,
// while every other write failure surfaces.
func createStreamToken(path string, token string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create stream id %q: %w", path, err)
	}
	// The token is written with a trailing newline so the file reads correctly
	// when a person cats it while debugging; parseStreamToken trims it back off.
	if _, err := file.WriteString(token + "\n"); err != nil {
		// Closed on the error path too — the deferred-close idiom would discard
		// this write error, which is the one that matters here.
		file.Close()
		return fmt.Errorf("write stream id %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close stream id %q: %w", path, err)
	}
	return nil
}

// newStreamToken mints a fresh opaque token. The bytes come from crypto/rand
// rather than a seeded source because tokens minted on two machines must not
// collide, and a process-seeded PRNG makes that a function of startup timing.
func newStreamToken() (string, error) {
	raw := make([]byte, streamTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate stream id: %w", err)
	}
	return strings.ToLower(streamTokenEncoding.EncodeToString(raw)), nil
}

// parseStreamToken turns the bytes of a token file into a StreamID, or fails.
// [LAW:parse-dont-validate] This is the only door from a string into a
// StreamID, so holding one IS the proof that the shape was checked; nothing
// downstream re-checks a token, because downstream there is no unchecked token
// left to check.
//
// The shape it admits is exactly the shape newStreamToken emits: precisely
// streamTokenLen characters drawn from the lowercase base32 alphabet. Anything
// else — an empty file, a truncated write, an editor's stray text, a token
// written by some future format — is rejected by name rather than carried
// forward as an identity that would attribute work to a checkout that never
// existed. The token is never decoded back into bytes (it is a discriminator,
// not a payload), so its length and alphabet are the whole of its contract.
func parseStreamToken(path string, raw string) (StreamID, error) {
	token := strings.TrimSpace(raw)
	if len(token) != streamTokenLen {
		return StreamID{}, fmt.Errorf("stream id %q is malformed: expected %d characters, found %d", path, streamTokenLen, len(token))
	}
	for _, char := range token {
		isLowerLetter := char >= 'a' && char <= 'z'
		isBase32Digit := char >= '2' && char <= '7'
		if !isLowerLetter && !isBase32Digit {
			return StreamID{}, fmt.Errorf("stream id %q is malformed: character %q is outside the token alphabet", path, char)
		}
	}
	return StreamID{value: token}, nil
}
