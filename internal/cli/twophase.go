package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/templates"
)

// [LAW:types-are-the-program] A two-phase command splits intent (preview) from
// commit (apply). The contract that binds them is a token derived purely from
// the plan — identical plans yield identical tokens, so the protocol is
// stateless: no nonce file, no TTL, no cross-process coordination. If anything
// in the plan drifts between preview and apply, the token mismatches and the
// apply phase refuses by construction.
//
// The token is intentionally short (8 hex chars / 32 bits) — collision risk is
// negligible for human-paced confirmations and the value is comfortable to
// copy from terminal output.
//
// applyToken hashes its parts in order, separated by a NUL byte to prevent
// boundary collisions (e.g. ["a", "bc"] vs ["ab", "c"]).
func applyToken(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:8]
}

// requireTokenPlaceholder enforces the contract that, in guided mode, the
// pre-guidance template communicates the apply token to the agent. A template
// that omits `<token>` would render a preview the agent cannot act on — apply
// would refuse with no way to discover the correct value. Failing loudly at
// load time, with a path to the offending override, is the single enforcement
// boundary for "guided pre-guidance must carry the token". The embedded
// default always satisfies this; only project/global overrides can violate it.
//
// [LAW:single-enforcer] One place validates the contract; all callsites trust
// that any pre-guidance reaching the render step contains `<token>`.
func requireTokenPlaceholder(preGuidance, action, workspaceRoot string) error {
	if strings.Contains(preGuidance, "<token>") {
		return nil
	}
	name := templates.GuidanceTemplateName(action, "pre")
	path, _, _ := templates.ActiveOverride(workspaceRoot, name)
	if path.IsEmpty() {
		// Defensive: the embedded default ships with `<token>`, so this branch
		// should be unreachable. Surface a precise diagnostic instead of a
		// silent fall-through if the embedded default is ever changed.
		return fmt.Errorf("pre-guidance template for `%s` is missing the required `<token>` placeholder (embedded default)", action)
	}
	return fmt.Errorf("pre-guidance template for `%s` is missing the required `<token>` placeholder; update %s to include `--apply=<token>` so the agent can discover the apply token", action, path)
}
