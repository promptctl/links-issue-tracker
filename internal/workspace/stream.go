package workspace

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
// The read-first step is a fast path, NOT the write-once enforcement — that
// distinction is the whole reason this is safe. Two commands starting together
// in a fresh checkout both read "absent" and both go on to publish; correctness
// comes from publishStreamToken's atomic link, which exactly one of them wins.
// The fast path only keeps the temp-file write and its fsync on a checkout's
// genuine first mutation instead of on every mutating command forever after.
//
// [LAW:one-source-of-truth] The final read is what every caller returns, so the
// FILE decides this checkout's identity — never the token this process happened
// to mint. A loser that returned its own unpublished candidate would stamp work
// with an identity no other command in the checkout agrees with.
func EnsureStream(privateGitDir string) (StreamID, error) {
	existing, err := ReadStream(privateGitDir)
	if err != nil {
		return StreamID{}, err
	}
	if existing.Present() {
		return existing, nil
	}
	if err := publishStreamToken(privateGitDir); err != nil {
		return StreamID{}, err
	}
	minted, err := ReadStream(privateGitDir)
	if err != nil {
		return StreamID{}, err
	}
	if !minted.Present() {
		// Reachable only if something removed the file between the publish above
		// and this read. Returning the absent StreamID would hand a mutating
		// command an identity-less pair that reads exactly like a never-mutated
		// checkout, so the anomaly stops here instead. [LAW:no-silent-failure]
		return StreamID{}, fmt.Errorf("stream id %q vanished immediately after it was written",
			filepath.Join(privateGitDir, streamTokenFile))
	}
	return minted, nil
}

// publishStreamToken mints a token and installs it write-once, in a way that
// makes the token file COMPLETE the instant it becomes visible.
//
// [LAW:no-ambient-temporal-coupling] Creating the destination and filling it are
// two moments, and every reader that arrives between them sees a file that
// exists and holds nothing. Creating the file at its final path first — an
// O_EXCL open followed by a write — publishes that empty moment to every other
// process, and the consequences are not hypothetical: a concurrent command
// reads the empty file and rejects it as malformed, and a process killed in
// that window leaves an empty file that can never be replaced, permanently
// failing every later read AND write for the checkout. So the token is written
// to a temp file, completed there, and only then given its real name.
//
// [LAW:single-enforcer] os.Link is what enforces write-once, and it is chosen
// over os.Rename deliberately: rename would silently REPLACE an existing
// identity, while link fails with EEXIST and leaves the incumbent alone. An
// already-published token is therefore a successful outcome here, not a
// failure — it is what every racing caller but one sees.
func publishStreamToken(privateGitDir string) error {
	token, err := newStreamToken()
	if err != nil {
		return err
	}
	// Created in the destination directory because a link cannot cross a
	// filesystem, and a temp dir elsewhere would put one there.
	temp, err := os.CreateTemp(privateGitDir, streamTokenFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary stream id in %q: %w", privateGitDir, err)
	}
	tempPath := temp.Name()
	// Removed whether this call wins or loses the race, and on every error path.
	// A process killed outright leaves its staged file behind, which is accepted
	// rather than reaped: the reaper would have to delete exactly the file a
	// CONCURRENT first-mutation has staged and is about to link, turning a
	// harmless orphan into a failed command, and an age threshold to avoid that
	// buys a tunable time constant to clean up files that barely occur. The read
	// -first path in EnsureStream means this function runs only while a checkout
	// has no identity at all, so the litter is bounded by kills inside one
	// checkout's first mutation, in a directory where git leaves index.lock under
	// the same conditions. Only the exact token name is ever read, so a leftover
	// staged file cannot be mistaken for an identity.
	defer os.Remove(tempPath)

	// The token is written with a trailing newline so the file reads correctly
	// when a person cats it while debugging; parseStreamToken trims it back off.
	if _, err := temp.WriteString(token + "\n"); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary stream id %q: %w", tempPath, err)
	}
	// CreateTemp opens at 0600; the token is not a secret and every other file
	// in the git directory is world-readable, so the mode is set explicitly
	// rather than left to differ from its neighbors by accident.
	if err := temp.Chmod(0o644); err != nil {
		temp.Close()
		return fmt.Errorf("set mode on temporary stream id %q: %w", tempPath, err)
	}
	// Synced before the link so a crash cannot leave the published name pointing
	// at unwritten bytes. The directory entry is deliberately NOT synced: losing
	// it to a crash means the token is simply absent and the next mutation mints
	// one, which is the safe failure, whereas losing the CONTENT would resurrect
	// exactly the permanently-empty file this design exists to make impossible.
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync temporary stream id %q: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary stream id %q: %w", tempPath, err)
	}

	path := filepath.Join(privateGitDir, streamTokenFile)
	err = os.Link(tempPath, path)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return publishFailure(path, privateGitDir, err)
	}
	return nil
}

// publishFailure names the one cause a caller cannot guess from the raw errno.
// Publishing needs a primitive that is atomic AND refuses an existing
// destination, and hard links are the portable one — but a filesystem that does
// not support them (FAT/exFAT, some network mounts) rejects every attempt, so
// every mutating command in that checkout fails forever with nothing but "link:
// operation not permitted" to go on. [LAW:no-silent-failure] The remedy is to
// say which capability is missing and where, not to fall back to a weaker
// publish: an implementation that is atomic on some filesystems and racy on
// others would leave nobody able to say what guarantee a given checkout has,
// and the racy one is precisely what this path was rewritten to eliminate.
func publishFailure(path string, privateGitDir string, err error) error {
	unsupported := []error{syscall.EPERM, syscall.EOPNOTSUPP, syscall.ENOSYS, syscall.EXDEV}
	for _, candidate := range unsupported {
		if errors.Is(err, candidate) {
			return fmt.Errorf("publish stream id %q: %w; this checkout's git directory (%s) is on a filesystem that does not support hard links, which lit requires to install the identity atomically", path, err, privateGitDir)
		}
	}
	return fmt.Errorf("publish stream id %q: %w", path, err)
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
//
// A damaged token fails every command in the checkout, read and write alike,
// so the error carries its own remedy — the file is the only state involved and
// deleting it restores a working checkout. It is deliberately NOT self-healed:
// quietly minting a replacement would give a second identity to a checkout
// whose first may already be attached to work, turning a loud, one-line fix
// into silently split attribution. [LAW:no-silent-failure]
func parseStreamToken(path string, raw string) (StreamID, error) {
	token := strings.TrimSpace(raw)
	if len(token) != streamTokenLen {
		return StreamID{}, malformedStreamToken(path,
			fmt.Sprintf("expected %d characters, found %d", streamTokenLen, len(token)))
	}
	for _, char := range token {
		isLowerLetter := char >= 'a' && char <= 'z'
		isBase32Digit := char >= '2' && char <= '7'
		if !isLowerLetter && !isBase32Digit {
			return StreamID{}, malformedStreamToken(path,
				fmt.Sprintf("character %q is outside the token alphabet", char))
		}
	}
	return StreamID{value: token}, nil
}

// malformedStreamToken states the damage and the fix in one message.
// [LAW:single-enforcer] Every rejection is phrased here, so no rejection can
// describe the damage without also telling the operator how to recover.
func malformedStreamToken(path string, detail string) error {
	return fmt.Errorf("stream id %q is malformed: %s; delete the file to mint a fresh identity for this checkout (work already recorded under the old identity keeps it, and any lane this checkout held is released)", path, detail)
}
