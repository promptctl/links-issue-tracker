package model

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"
)

// ConfigApply is one apply of the project config: the whole bytes of the
// committed .lit/config.toml, copied into the store so every clone and every
// reader runs on the same config. The file in git is the intent; the apply is
// what is in force. Applies are never edited or removed, so the store's
// applies are a history, and the newest of them is the config in force
// (ConfigInForce).
//
// It holds the bytes, never a separate name for them: the digest is read off
// the bytes, so an apply whose name and content disagree is unrepresentable.
// [LAW:one-source-of-truth]
type ConfigApply struct {
	ID        ID        `json:"id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy Text      `json:"created_by"`
}

// Digest names the apply's content.
func (a ConfigApply) Digest() ConfigDigest { return DigestConfig(a.Content) }

// ConfigDigest names one config text: the hex SHA-256 of it.
type ConfigDigest string

// DigestConfig is the one way a config text becomes its ConfigDigest.
// [LAW:single-enforcer]
func DigestConfig(content string) ConfigDigest {
	sum := sha256.Sum256([]byte(content))
	return ConfigDigest(hex.EncodeToString(sum[:]))
}

// Short is the digest as a person reads it: its first twelve digits.
func (d ConfigDigest) Short() string { return string(d)[:12] }

// ConfigApplyOrdering is the order an export carries applies in, oldest
// first: the instant, then the id — CompareEventRecency's rule, so the newest
// apply is the one every store agrees is newest.
func ConfigApplyOrdering(a, b ConfigApply) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID.Raw(), b.ID.Raw()))
}

// ConfigInForce is the apply in force among applies, the newest by
// ConfigApplyOrdering, and false when nothing has been applied: the store then
// runs on lit's defaults. Every store that holds the same applies picks the
// same one, whatever order a sync delivered them in.
func ConfigInForce(applies []ConfigApply) (ConfigApply, bool) {
	if len(applies) == 0 {
		return ConfigApply{}, false
	}
	return slices.MaxFunc(applies, ConfigApplyOrdering), true
}
