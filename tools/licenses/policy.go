package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	lc "github.com/google/licenseclassifier"
)

//go:embed policy.json
var policyJSON []byte

// Policy is the committed license policy (policy.json): the permissive license
// names permitted for any linked module, plus per-module exceptions for modules
// whose classified license isn't in that set but has been human-verified as
// permissive. [LAW:one-source-of-truth] policy.json is the one place the
// accept/reject rule is stated, and it is embedded so the running gate and the
// committed file can never drift.
type Policy struct {
	Note             string            `json:"note"`
	AllowedLicenses  []string          `json:"allowed_licenses"`
	ModuleExceptions []ModuleException `json:"module_exceptions"`
}

// ModuleException accepts one specific (module, classified-license) pair. The
// license is part of the key on purpose: an exception grants a SPECIFIC license
// for a SPECIFIC module, so if the module later reclassifies (e.g. fslock's
// LGPL-with-static-exception becomes plain GPL after an upstream relicense), the
// exception stops matching and the module re-violates loudly instead of riding a
// stale pass. [LAW:no-silent-failure]
type ModuleException struct {
	Module  string `json:"module"`
	License string `json:"license"`
	Reason  string `json:"reason"`
}

// LoadPolicy parses the embedded policy.json.
func LoadPolicy() (*Policy, error) {
	return parsePolicy(policyJSON)
}

// parsePolicy is the one boundary that turns policy bytes into a *Policy the
// gate may run against; the checks below are part of the parse, so no
// malformed policy ever exists as a Policy value. [LAW:parse-dont-validate]
func parsePolicy(data []byte) (*Policy, error) {
	var p Policy
	// The policy is a hand-edited committed file, and Unmarshal's default of
	// ignoring unknown keys would turn a misspelled "module_exceptions" into
	// an empty list — a silently vanished exception row. Unknown keys fail
	// the parse instead. [LAW:no-silent-failure]
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse embedded policy.json: %w", err)
	}
	// Decode reads one JSON value; a concatenated second document or paste
	// artifact after it would otherwise be silently dropped and the gate
	// would run against a partial read of the committed file.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("policy.json carries trailing content after the policy object; refusing a file the gate would only partially read")
	}
	// encoding/json keeps the LAST value of a duplicated key, so a bad merge
	// leaving two module_exceptions keys would silently drop one — the same
	// committed-file-vs-gate's-view split the unknown-key and trailing-content
	// guards refuse. Same fate for duplicates. [LAW:no-silent-failure]
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, err
	}
	// [LAW:no-silent-failure] an empty allowlist would make the gate reject
	// every module — almost certainly a broken/truncated policy file, not a
	// real intent. Refuse rather than fail the whole tree confusingly.
	if len(p.AllowedLicenses) == 0 {
		return nil, fmt.Errorf("policy.json has no allowed_licenses; refusing to run a gate that would reject every module")
	}
	if err := checkAllowedLicenses(p.AllowedLicenses); err != nil {
		return nil, err
	}
	// [LAW:parse-dont-validate] An exception without a module, license, and
	// human-verified reason is the undocumented excuse this policy exists to
	// prevent; the gate must never run against one. Enforced here at the one
	// parse point, so the rule holds however the committed list changes —
	// today's list is empty and a test pins that, but the pin is a policy
	// stance, not this invariant's enforcer.
	seenExceptions := make(map[exKey]bool, len(p.ModuleExceptions))
	for _, e := range p.ModuleExceptions {
		// The committed file is the canonical form the gate runs against, so
		// a padded field is rejected rather than silently trimmed — Filter
		// keys exceptions on these exact strings, and a normalize-on-read
		// would split the file's text from the value it produces.
		// [LAW:one-source-of-truth]
		for _, field := range []string{e.Module, e.License, e.Reason} {
			if field != strings.TrimSpace(field) {
				return nil, fmt.Errorf("policy.json module_exception %+v carries surrounding whitespace in a field; the committed text must be the exact value the gate matches", e)
			}
		}
		if e.Module == "" || e.License == "" || e.Reason == "" {
			return nil, fmt.Errorf("policy.json module_exception %+v is missing module, license, or reason; every exception must be complete and human-verified", e)
		}
		// Same reasoning as the allowlist's duplicate rule, with a sharper
		// edge: Filter keys exceptions on (module, license), so two rows
		// differing only in their reason collapse onto ONE key — and -check's
		// green line publishes len(ModuleExceptions) as a fact, so the printed
		// count would exceed the exceptions the gate actually holds. Two
		// reasons for one grant is also a question in its own right.
		// [LAW:no-silent-failure]
		if seenExceptions[exKey{e.Module, e.License}] {
			return nil, fmt.Errorf("policy.json carries two module_exceptions for %s under %q; Filter keys them on that pair, so the second is invisible to the gate while -check still counts it", e.Module, e.License)
		}
		seenExceptions[exKey{e.Module, e.License}] = true
		// An exception records a human having READ a license and found it
		// permissive. A sentinel is this tool reporting there was nothing
		// legible to read, so an exception naming one claims a reading that
		// cannot have happened — refused by refuseSentinel inside the call
		// below, which is also where a sentinel hiding as an arm base gets
		// caught. [LAW:no-silent-failure]
		//
		// The expression rules are about this FILE, not about the allowlist,
		// so they hold on an exception's license too. Without this the OR ban
		// had a door beside it: an exception reading "BSD-3-Clause OR
		// GPL-2.0-only" — zstd's own un-elected upstream grant, and exactly
		// the un-made election the ban exists to refuse — parsed clean and
		// became a live key in Filter's exception table, while the identical
		// string in allowed_licenses was rejected. What does NOT apply here is
		// the AND-arm vetting rule: an exception is not a permission granted
		// to a license, it is one module's grant a human read, so requiring
		// its arms to be allowlisted would contradict its whole purpose.
		if _, err := parseLicenseExpression("policy.json module_exception license", e.License); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

// checkAllowedLicenses is the allowlist's half of the parse: every rule that
// makes an entry mean what it says, settled before any Policy value exists.
// [LAW:parse-dont-validate]
func checkAllowedLicenses(allowed []string) error {
	set := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		// The allowlist is matched by exact string the same way exceptions
		// are, so its entries carry the same rule: the committed text is the
		// value the gate compares, and a padded entry is refused rather than
		// silently trimmed, because a normalize-on-read would split the
		// file's text from the value it produces. [LAW:one-source-of-truth]
		if name == "" || name != strings.TrimSpace(name) {
			return fmt.Errorf("policy.json allowed_licenses entry %q is blank or carries surrounding whitespace; the committed text must be the exact license name the gate matches", name)
		}
		// A repeated entry is a merge artifact, and this change made the
		// LENGTH of this list load-bearing in two places that are read as
		// facts: -check's green line prints it, and the note's re-measure
		// procedure says the distinct licenses in LICENSE-REPORT.md's summary
		// are exactly these entries. A duplicate makes both statements false
		// while changing nothing about what the gate permits, which is the
		// quietest kind of wrong. [LAW:no-silent-failure]
		if set[name] {
			return fmt.Errorf("policy.json allowed_licenses repeats %q; the list's length is printed as a fact by -check and read as one by its note, so a duplicate makes both wrong while permitting nothing new", name)
		}
		set[name] = true
	}
	for _, name := range allowed {
		// The two rules below are the allowlist's alone. An entry here is a
		// PERMISSION, so its arms have to be independently acceptable and its
		// license has to be one this repository will actually accept. An
		// exception's license is not a permission — it names one module's
		// grant that a human read, and naming a license the allowlist refuses
		// is the only reason to write one — so it gets the shape rules from
		// parseLicenseExpression and neither of these.
		arms, err := parseLicenseExpression("policy.json allowed_licenses entry", name)
		if err != nil {
			return err
		}
		// Both halves of every arm, not just the bases. The exception half is
		// the one that reads like a formality and is not: Commons-Clause is an
		// SPDX exception that removes the right to sell, and the classifier
		// types it FORBIDDEN, so "Apache-2.0 WITH Commons-Clause" is a
		// narrowed grant wearing a permissive base.
		//
		// Applying this to an AND-arm's base is, today, unreachable by any
		// input the rule below does not already refuse: a compound's arms must
		// each be present as their own entry, and each entry is vetted here in
		// its own right. That redundancy is deliberate rather than dead — the
		// two rules answer different questions, and only this one still holds
		// if the AND-arm requirement is ever relaxed.
		for _, arm := range arms {
			for _, identifier := range []string{arm.base, arm.exception} {
				if identifier == "" {
					continue
				}
				if err := refuseCopyleftAllowlistEntry(name, identifier); err != nil {
					return err
				}
			}
		}
		if len(arms) == 1 {
			continue
		}
		for _, arm := range arms {
			if !set[arm.base] {
				return fmt.Errorf("policy.json allowed_licenses entry %q has an AND-arm granting under %q, which is not itself allowlisted; an AND grants under every arm at once, so each one must be independently acceptable — add %q on its own line (the base identifier alone, with any WITH-exception dropped) or drop the expression", name, arm.base, arm.base)
			}
		}
	}
	return nil
}

// copyleftLicenseTypes are the licenseclassifier taxonomy buckets an
// allowlist entry may never fall in: "restricted" (GPL, LGPL, OSL),
// "reciprocal" (MPL, EPL, CDDL) and "FORBIDDEN" (AGPL, and — see below —
// Commons-Clause).
//
// This is a VETO and never the authority, and being exact about the
// difference matters more than the veto itself, because an overstated
// backstop is worse than none. Two measured limits:
//
// The taxonomy is a corporate policy, not a copyleft test. It calls WTFPL
// "FORBIDDEN" though WTFPL is about as permissive as a license gets, and it
// has no opinion at all about several licenses this repository legitimately
// ships — Unicode-3.0 returns "", as does every compound expression. So a
// positive rule ("must classify permissive") would reject policy.json itself,
// and only the negative form is usable.
//
// And its coverage has holes that are NOT closed by spelling normalization.
// EUPL-1.1, CPAL-1.0, SISSL, LPPL-1.3c and LGPLLR are files in the
// classifier's own corpus, so Classify emits those exact strings, and
// LicenseType returns "" for every one of them — several are plainly
// reciprocal. This veto would not stop any of them being allowlisted. It
// catches what nobody here would defend and it is not a proof; the rule that
// this list is permissive-only still needs a reader, which is why the note
// says so in prose as well.
var copyleftLicenseTypes = map[string]bool{"restricted": true, "reciprocal": true, "FORBIDDEN": true}

// spdxDeprecatedSpelling maps a current SPDX identifier onto the deprecated
// one licenseclassifier's corpus is named after, because the corpus predates
// the 2017 rename and LicenseType is a lookup in it. Measured: "GPL-3.0"
// types "restricted" while "GPL-3.0-only", "GPL-3.0-or-later", "GPL-2.0+",
// "LGPL-2.1-only" and "AGPL-3.0-only" all type "". Without this the veto
// would refuse only the deprecated half of every copyleft family it claims —
// and native.go's hand-authored license strings already use the current
// spelling (zstd's un-elected arm is written "GPL-2.0-only" there), so the
// modern form is the one that would actually arrive.
//
// Mechanical rather than a hand-listed table on purpose: the rename is a
// suffix rule, so a family added to the corpus later is covered without an
// edit here.
func spdxDeprecatedSpelling(id string) string {
	for _, suffix := range []string{"-or-later", "-only"} {
		if trimmed := strings.TrimSuffix(id, suffix); trimmed != id {
			return trimmed
		}
	}
	return strings.TrimSuffix(id, "+")
}

// refuseCopyleftAllowlistEntry turns "every entry is permissive" from a claim
// the note makes into a rule the parse enforces, as far as it reaches. Before
// it, the only thing standing between allowed_licenses and a GPL entry was a
// reader — which is precisely what this file's note spends a paragraph saying
// not to trust, about a different array.
func refuseCopyleftAllowlistEntry(entry, identifier string) error {
	for _, spelling := range []string{identifier, spdxDeprecatedSpelling(identifier)} {
		kind := lc.LicenseType(spelling)
		if !copyleftLicenseTypes[kind] {
			continue
		}
		return fmt.Errorf("policy.json allowed_licenses entry %q involves %q, which the classifier types as %q; this list is permissive-only and adding a copyleft license to it is not the way past a red gate — remove the dependency instead",
			entry, identifier, kind)
	}
	return nil
}

// parseLicenseExpression reads one license-valued field of policy.json as an
// SPDX expression and returns the base identifier of each of its AND-arms —
// the identifiers the expression grants under, with any WITH-exception
// stripped, since an exception only ever WIDENS a grant. A single-element
// result means the field is not a combination at all.
//
// Everything it refuses is a way for the committed text and the value the gate
// actually matches to come apart, which is this file's recurring failure mode:
//
//   - Whitespace that is not single spaces. The arms are read through
//     strings.Fields, which collapses runs of spaces, tabs and newlines, while
//     Filter matches the entry BYTE for byte. Re-wrapping or re-indenting an
//     entry would otherwise pass this parse and every test, then fail the gate
//     naming a string the file visibly appears to carry. Same reasoning as the
//     padded-entry refusal in checkAllowedLicenses: a normalize-on-read splits
//     the file's text from the value it produces. [LAW:one-source-of-truth]
//   - An OR. lit resolves a dual license to the ELECTED arm before policy ever
//     sees it, recording one identifier plus acknowledgement "concluded"
//     (zstd, native.go). An OR reaching this file means an election went
//     unmade, which is the ambiguity the shipped artifacts exist to remove.
//   - Parentheses, for the same reason: in SPDX they exist to group an OR,
//     since grouping ANDs changes nothing.
//   - An operator spelled in any case but its canonical upper. SPDX operators
//     are uppercase, and a lowercase "or" read as an identifier token would
//     slip a dual license past the OR ban while three documents say it cannot.
//   - An arm that is neither an identifier nor "<identifier> WITH <exception>".
//     The gate matches by exact string and will not rule on an expression it
//     had to guess the meaning of. [LAW:no-silent-failure]
//
// licenseArm is one AND-arm of a license expression, decomposed into the
// identifier it grants under and the SPDX exception attached to it (empty
// when there is none). Both are carried because both must be vetted: an
// earlier draft returned bases alone on the premise that "a WITH-exception
// only widens a grant", and that premise is false — Commons-Clause is an
// SPDX exception that REMOVES the right to sell, and the classifier types it
// FORBIDDEN. An unexamined right-hand token is a second license riding in on
// the first.
type licenseArm struct{ base, exception string }

func parseLicenseExpression(where, name string) ([]licenseArm, error) {
	// Ahead of every shape rule, because the sentinel's own spelling is
	// "Skipped (oversize)" and the parenthesis rule below would otherwise
	// answer it with advice about grouping ORs. The diagnostic a hand-edited
	// compliance file gives back is part of what the file is for.
	if err := refuseSentinel(where, name); err != nil {
		return nil, err
	}
	// Ahead of the character rule below, which would otherwise answer a
	// parenthesis with a general complaint about characters SPDX does not
	// contain. Parentheses are worth their own sentence: they are valid SPDX,
	// they group an OR, and saying so tells the editor what is actually wrong.
	if strings.ContainsAny(name, "()") {
		return nil, fmt.Errorf("%s %q is parenthesized; SPDX parentheses group an OR, and an elected license reaches this file as a single identifier", where, name)
	}
	// Ahead of the whitespace rule, not after it. Several of the invisible
	// characters worth refusing — U+00A0 above all — satisfy unicode.IsSpace,
	// so strings.Fields splits on them and the Join comparison below fails
	// first, answering a non-breaking space with advice about repeated or
	// wrapped whitespace. That diagnostic is not merely unhelpful, it is
	// false, and it points the editor at the one thing that is not wrong.
	//
	// SPDX identifiers are drawn from letters, digits, `.`, `-` and `+`, and
	// an expression joins them with single spaces. Everything else is refused
	// because it is invisible or near-invisible in an editor, survives every
	// check that tokenizes rather than compares, and leaves the gate reporting
	// a non-allowlisted license naming a string the file appears to carry — a
	// zero-width space inside "MIT", a full-width parenthesis, a non-breaking
	// space between arms. [LAW:one-source-of-truth]
	for _, r := range name {
		if !isSPDXRune(r) {
			return nil, fmt.Errorf("%s %q carries the character %q (U+%04X), which no SPDX identifier or expression contains; the gate compares this file byte for byte and would report the entry as non-allowlisted while it looks correct on screen", where, name, r, r)
		}
	}
	fields := strings.Fields(name)
	if strings.Join(fields, " ") != name {
		return nil, fmt.Errorf("%s %q carries repeated, tabbed, or wrapped whitespace; the gate matches this file byte for byte, so this would parse here and then be reported as a non-allowlisted license naming a string the file appears to carry", where, name)
	}
	var arms [][]string
	arm := []string{}
	for _, field := range fields {
		// [LAW:one-source-of-truth] the operator SET is spdxOperators, the
		// SBOM's arm discriminator (sbom.go), so a token added there is an
		// operator here too and neither file keeps its own list.
		//
		// Only the set is shared, and the two LOOKUPS differ on purpose:
		// sbom.go tests the raw token because it must render whatever string
		// it is handed, while this upper-cases first in order to CATCH a
		// non-canonical spelling and refuse it two cases below. They also
		// answer different questions — the SBOM asks "is this an expression",
		// this asks "is it a combination" — which is why "Apache-2.0 WITH
		// LLVM-exception" is an expression there and a single arm here.
		canonical := strings.ToUpper(field)
		switch {
		case !spdxOperators[canonical]:
			arm = append(arm, field)
		case field != canonical:
			return nil, fmt.Errorf("%s %q spells the SPDX operator %q in other than its canonical upper case; SPDX operators are uppercase, and a lowercase one read as an identifier would carry a dual license straight past the OR refusal below", where, name, field)
		case canonical == "OR":
			return nil, fmt.Errorf("%s %q carries an OR; a dual license is resolved to its elected arm before the gate sees it (recorded as one identifier plus acknowledgement %q), so an OR here means an election went unmade", where, name, acknowledgementConcluded)
		case canonical == "AND":
			arms = append(arms, arm)
			arm = nil
		default: // WITH binds to the arm it widens.
			arm = append(arm, field)
		}
	}
	arms = append(arms, arm)

	parsed := make([]licenseArm, 0, len(arms))
	for _, arm := range arms {
		var a licenseArm
		switch {
		case len(arm) == 1:
			a = licenseArm{base: arm[0]}
		case len(arm) == 3 && arm[1] == "WITH":
			a = licenseArm{base: arm[0], exception: arm[2]}
		default:
			return nil, fmt.Errorf("%s %q has an arm %q that is neither an SPDX identifier nor an identifier WITH an exception; the gate matches this file by exact string and will not rule on an expression it cannot decompose", where, name, strings.Join(arm, " "))
		}
		// Shape was checked by token COUNT, which says nothing about what the
		// tokens ARE. Both halves of an arm are checked, and the exception
		// half is not the formality it reads as: "Apache-2.0 WITH GPL-3.0"
		// otherwise carried a string past this parse that is refused outright
		// two rules away, and "Unknown WITH some-exception" slipped the
		// sentinel through as a base. A bare operator passes the count too —
		// "WITH" alone is one token and read as an identifier.
		for _, identifier := range []string{a.base, a.exception} {
			if identifier == "" {
				continue
			}
			if spdxOperators[identifier] {
				return nil, fmt.Errorf("%s %q uses the SPDX operator %q where an identifier belongs; an operator is not a license name", where, name, identifier)
			}
			if err := refuseSentinel(where, identifier); err != nil {
				return nil, err
			}
		}
		parsed = append(parsed, a)
	}
	return parsed, nil
}

// isSPDXRune reports whether r may appear in an SPDX identifier or in the
// expression syntax joining identifiers. Deliberately a closed set rather
// than a unicode category test: the values this refuses are the ones that
// LOOK right in an editor, so "is it printable" is the wrong question.
func isSPDXRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return r == '.' || r == '-' || r == '+' || r == ' '
}

// refuseSentinel rejects one of this tool's own no-verdict markers wherever a
// license name is expected. [LAW:one-source-of-truth] both fields of
// policy.json and every decomposed arm reach it here, so the rule is stated
// once — an earlier draft spelled it separately for allowed_licenses and for
// module_exceptions and still missed the arm.
func refuseSentinel(where, name string) error {
	if !licenseSentinels[name] {
		return nil
	}
	return fmt.Errorf("%s names %q, which is not a license but this tool's marker for having no verdict on one; an unidentifiable license is the one row an auditor cannot evaluate at all, and it has neither an allowlist nor an exception path — remove the dependency instead", where, name)
}

// policyKeys are every key this schema has, in the one spelling each is
// written in. Checked byte for byte by rejectDuplicateKeys; see the comment
// there for why matching encoding/json's own case-folding was the wrong
// repair. [LAW:one-source-of-truth] these are the json tags on Policy and
// ModuleException, and a field added to either must be added here — the
// unknown-key error names the whole set, so the omission announces itself the
// first time the new key is used.
var policyKeys = map[string]bool{
	"note": true, "allowed_licenses": true, "module_exceptions": true,
	"module": true, "license": true, "reason": true,
}

// rejectDuplicateKeys walks the document's tokens and errors on any key
// repeated within one object, at any depth. encoding/json's decoder accepts
// duplicates and keeps the last value, which would let half of a bad merge
// vanish from the gate's view while staying in the committed text.
func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	// seen is a stack of per-object key sets; a nil entry marks an array
	// level, whose elements are values rather than key/value pairs.
	var seen []map[string]bool
	expectKey := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("parse embedded policy.json: %w", err)
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				seen = append(seen, map[string]bool{})
				expectKey = true
			case '[':
				seen = append(seen, nil)
				expectKey = false
			case '}', ']':
				seen = seen[:len(seen)-1]
				expectKey = len(seen) > 0 && seen[len(seen)-1] != nil
			}
		case string:
			top := len(seen) - 1
			if expectKey && top >= 0 && seen[top] != nil {
				// The key must be one of the six spellings this schema
				// actually has, EXACTLY. That is a stronger rule than
				// deduplication and it is here because deduplication alone
				// could not close the hole: encoding/json resolves a key to a
				// struct field case-insensitively as a fallback, so
				// "ALLOWED_LICENSES" is not an unknown field and
				// DisallowUnknownFields never fires on it, while a walk
				// comparing raw text sees two unrelated strings and no
				// duplicate. A policy could therefore show a reader
				// `"module_exceptions": []` beside a `"MODULE_EXCEPTIONS"`
				// holding a live exception, and the gate would run on the
				// second.
				//
				// The obvious repair — lower-case both before comparing — is
				// not equivalent to what the decoder does: its fold is
				// Unicode-aware in ways strings.ToLower is not, so matching it
				// means reimplementing it and staying in step with it. A
				// closed schema needs neither. Six known keys, compared byte
				// for byte, and anything else is refused whatever the decoder
				// would have made of it. [LAW:parse-dont-validate]
				if !policyKeys[t] {
					return fmt.Errorf("policy.json carries the key %q, which is not one of this file's keys (note, allowed_licenses, module_exceptions, module, license, reason); encoding/json would resolve a case-variant onto a real field and the committed text would stop describing what the gate reads", t)
				}
				if seen[top][t] {
					return fmt.Errorf("policy.json repeats the key %q within one object; a duplicated key silently drops one of its values from the gate's view", t)
				}
				seen[top][t] = true
				expectKey = false
			} else {
				expectKey = len(seen) > 0 && seen[len(seen)-1] != nil
			}
		default:
			// A non-string value token completes a key/value pair (or is an
			// array element); inside an object the next token is a key again.
			expectKey = len(seen) > 0 && seen[len(seen)-1] != nil
		}
	}
}

// Violation is one linked module whose classified license is neither allowed nor
// excepted.
type Violation struct {
	Module  string
	Version string
	License string
}

// exKey is the (module, license) identity of an exception — see ModuleException
// for why the license is part of the key.
type exKey struct{ module, license string }

// LicenseFilter is the policy's accept/reject rule with its lookup tables built:
// the one place in this tool that decides whether a (module, license) pair is
// acceptable. It exists as a type rather than as a loop inside CheckPolicy
// because two callers now need that ruling — the link-closure gate, which turns
// a rejection into a build failure, and the module-graph audit, which turns one
// into a reported row. Extracting it means "permissive" cannot come to mean two
// slightly different things in the two places that use the word.
// [LAW:single-enforcer]
type LicenseFilter struct {
	allowed  map[string]bool
	excepted map[exKey]bool
}

// licenseSentinels are the values this tool records IN PLACE OF a license name
// when it has no verdict on one: unclassifiedLicense, where the text matched
// nothing above the classifier's confidence threshold, and oversizeLicense,
// where the file was too large to have been read at all. Neither names a
// grant, so neither can be allowlisted or excepted — an unidentifiable license
// is the single row an auditor cannot evaluate, and a policy that waves one
// through has stopped being a policy.
//
// [LAW:one-source-of-truth] this is the package's ONE enumeration of "the tool
// has no verdict here" — isLicenseText (graph.go) and partitionGraph's
// unclassified case (graph_report.go) both read it rather than re-listing the
// two constants, which is how they used to be written. A third sentinel added
// to this map is therefore barred from the policy, judged by isLicenseText
// under the rule for a file nobody could read rather than the rule for a
// recognised grant, and filed under the report's unclassified section — all
// without a second edit to remember.
//
// Note which way that middle one runs: membership here makes a hit LESS
// automatically kept, not more. isLicenseText keeps anything the classifier
// recognised whatever the file is called, and gates everything else on the
// extension, because an unreadable `.go` fixture is machine content while an
// unreadable `COPYING` is the worst row in the audit.
var licenseSentinels = map[string]bool{unclassifiedLicense: true, oversizeLicense: true}

// Filter compiles the policy into its accept/reject rule once, so a caller
// ruling on hundreds of rows does not rebuild the lookup tables per row.
// [LAW:parse-dont-validate] the returned value is the policy already
// interpreted, and no RULING re-reads AllowedLicenses or ModuleExceptions.
// runCheck's green line does read both slices, for their lengths — it reports
// the shape of the policy rather than applying it, which is why it reads the
// raw values and why that is not a second accept/reject path.
//
// It copies the policy's entries verbatim, sentinels included. Dropping them
// here was the first shape of the sentinel ban and it was the wrong one: it
// would have let a Policy value and the LicenseFilter built from it disagree
// about what the file says, silently — the same committed-text-versus-gate's-
// view split that the unknown-key, trailing-content, and duplicate-key guards
// in parsePolicy all exist to refuse. The ban belongs where the ruling
// happens; see Allows and Permits.
func (p *Policy) Filter() LicenseFilter {
	allowed := make(map[string]bool, len(p.AllowedLicenses))
	for _, name := range p.AllowedLicenses {
		allowed[name] = true
	}
	excepted := make(map[exKey]bool, len(p.ModuleExceptions))
	for _, e := range p.ModuleExceptions {
		excepted[exKey{e.Module, e.License}] = true
	}
	return LicenseFilter{allowed: allowed, excepted: excepted}
}

// Allows reports whether license is in the policy's allowlist. Separate from
// Permits because the two accept shapes have different reach: an allowlisted
// license is permissive wherever it turns up, whereas an exception was granted
// after a human read one specific file. The module-graph audit needs to tell
// those apart; the link-closure gate, where every entry is a module's own
// grant, does not care.
//
// A sentinel is refused ahead of the lookup, and both rulings carry the same
// refusal rather than sharing one, because both are entry points: permitsHit
// (graph_report.go) calls Allows directly. Stating it at the rulings — rather
// than by omitting the key when the tables are built — is what makes "no
// policy permits an unreadable license" true of every LicenseFilter, including
// one a future caller assembles as a struct literal in this package, instead
// of true only of the ones Filter produced. [LAW:single-enforcer]
func (f LicenseFilter) Allows(license string) bool {
	return !licenseSentinels[license] && f.allowed[license]
}

// Permits reports whether license is acceptable as module's own license grant:
// it is in AllowedLicenses, or the (module, license) pair carries a
// ModuleException. [LAW:types-are-the-program] the two accept shapes are
// enumerated here, so the reject case is exactly everything that is neither.
// A module the classifier could not read is not one of the two and cannot be
// made into one — see licenseSentinels for why that is a hard failure rather
// than a documentable exception.
func (f LicenseFilter) Permits(module, license string) bool {
	if licenseSentinels[license] {
		return false
	}
	return f.Allows(license) || f.excepted[exKey{module, license}]
}

// CheckPolicy is the accept/reject predicate over the whole inventory: a module
// passes iff LicenseFilter permits its classified license. Everything else is a
// violation. Pure and order-deterministic (violations follow entries' order).
func CheckPolicy(entries []Entry, p *Policy) []Violation {
	filter := p.Filter()
	var violations []Violation
	for _, e := range entries {
		if filter.Permits(e.Module.Path, e.LicenseName) {
			continue
		}
		violations = append(violations, Violation{Module: e.Module.Path, Version: e.Module.Version, License: e.LicenseName})
	}
	return violations
}

// runCheck is the CLI entry for the license-policy gate: build the inventory,
// load the policy, and fail loudly listing every module outside it. It writes no
// artifacts — the gate only reads — and shares buildEntries with the generator,
// so it checks the exact licenses the report documents. [LAW:effects-at-boundaries]
// [LAW:single-enforcer]
func runCheck(pkg string, stdout io.Writer) error {
	entries, err := buildEntries(pkg)
	if err != nil {
		return err
	}
	policy, err := LoadPolicy()
	if err != nil {
		return err
	}
	violations := CheckPolicy(entries, policy)
	if len(violations) > 0 {
		// [LAW:no-silent-failure] name every offending module + license so a
		// human can allowlist the license (if permissive) or replace the
		// dependency — never a bare non-zero exit.
		sort.Slice(violations, func(i, j int) bool { return violations[i].Module < violations[j].Module })
		var b strings.Builder
		fmt.Fprintf(&b, "license policy: %d linked module(s) under a non-allowlisted license:\n", len(violations))
		for _, v := range violations {
			fmt.Fprintf(&b, "  - %s@%s: %s\n", v.Module, v.Version, v.License)
		}
		// The remediation names removal FIRST on purpose. module_exceptions is
		// empty by design (see policy.json's note), and the failure this gate
		// exists to prevent is a future editor, mid-task and under time
		// pressure, documenting their way past it with a persuasive reason.
		// Pointing them at an exception as the ordinary next step is how that
		// happens. An "Unknown" row has no such step at all.
		b.WriteString("Resolve by removing the dependency, or — if the license is genuinely permissive and something lit ships now carries it — adding it to allowed_licenses in tools/licenses/policy.json. If that license is a compound expression, add each AND-arm's BASE identifier on its own line too — the bare identifier, with any WITH-exception dropped — or the file stops loading at all and takes -check and -graph down with it. A module reported as \"Unknown\" carries neither route: nobody can say what its license permits, so it must go.")
		return fmt.Errorf("%s", b.String())
	}
	// The green line says what the gate proved, and it must not advertise the
	// route the red line above stopped offering: this is the string a release
	// run prints into a log an auditor reads, and it is the only place that
	// audience hears from the gate at all. It names the exception count rather
	// than the word "excepted" so that a reader learns the array is empty from
	// the gate's own output — the guarantee FORKS.md says a green -check
	// carries — instead of having to go and check.
	fmt.Fprintf(stdout, "license policy OK: all %d components cleared the policy's %d allowlisted licenses and %d module exceptions\n",
		len(entries), len(policy.AllowedLicenses), len(policy.ModuleExceptions))
	return nil
}
