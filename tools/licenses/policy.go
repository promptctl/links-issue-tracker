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
		// An exception records a human having READ a license and found it
		// permissive. A sentinel is this tool reporting there was nothing
		// legible to read, so an exception naming one claims a reading that
		// cannot have happened. Filter drops it from the lookup table
		// regardless (see licenseSentinels); refusing it here keeps the
		// committed file from stating a rule that has no effect.
		// [LAW:no-silent-failure]
		if licenseSentinels[e.License] {
			return nil, fmt.Errorf("policy.json module_exception for %s names %q, which is not a license but this tool's marker for having no verdict on one; an unidentifiable license has no exception path — remove the dependency instead", e.Module, e.License)
		}
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
		// Allowlisting a sentinel says "whatever we could not identify is
		// fine" — the precise inverse of a policy. Same reasoning as the
		// exception guard above, and the same reason it is refused rather
		// than ignored.
		if licenseSentinels[name] {
			return fmt.Errorf("policy.json allowed_licenses names %q, which is not a license but this tool's marker for having no verdict on one; an unidentifiable license is the one row an auditor cannot evaluate at all, and it has no allowlist path", name)
		}
		set[name] = true
	}
	for _, name := range allowed {
		// The vetting rule below is the allowlist's alone: an expression entry
		// here permits the COMBINATION, so each arm has to be independently
		// acceptable. An exception's license is not a permission at all — it
		// names one module's grant a human read — so it gets the shape rules
		// and not this one.
		bases, err := parseLicenseExpression("policy.json allowed_licenses entry", name)
		if err != nil {
			return err
		}
		if len(bases) == 1 {
			continue
		}
		for _, base := range bases {
			if !set[base] {
				return fmt.Errorf("policy.json allowed_licenses entry %q has an AND-arm granting under %q, which is not itself allowlisted; an AND grants under every arm at once, so each one must be independently acceptable — add %q on its own line or drop the expression", name, base, base)
			}
		}
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
func parseLicenseExpression(where, name string) ([]string, error) {
	fields := strings.Fields(name)
	if strings.Join(fields, " ") != name {
		return nil, fmt.Errorf("%s %q carries repeated, tabbed, or wrapped whitespace; the gate matches this file byte for byte, so this would parse here and then be reported as a non-allowlisted license naming a string the file appears to carry", where, name)
	}
	if strings.ContainsAny(name, "()") {
		return nil, fmt.Errorf("%s %q is parenthesized; SPDX parentheses group an OR, and an elected license reaches this file as a single identifier", where, name)
	}
	var arms [][]string
	arm := []string{}
	for _, field := range fields {
		// [LAW:one-source-of-truth] spdxOperators is the SBOM's arm
		// discriminator (sbom.go); recognizing operators from that one set
		// keeps the gate and the rendered document from disagreeing about
		// which tokens are operators at all. What each does with the answer
		// still differs, and should: the SBOM asks "is this an expression",
		// this asks "is it a combination".
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

	bases := make([]string, 0, len(arms))
	for _, arm := range arms {
		switch {
		case len(arm) == 1:
			bases = append(bases, arm[0])
		case len(arm) == 3 && arm[1] == "WITH":
			bases = append(bases, arm[0])
		default:
			return nil, fmt.Errorf("%s %q has an arm %q that is neither an SPDX identifier nor an identifier WITH an exception; the gate matches this file by exact string and will not rule on an expression it cannot decompose", where, name, strings.Join(arm, " "))
		}
	}
	return bases, nil
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
// to this map is therefore barred from the policy, kept as a license hit
// whatever the file is named, and filed under the report's unclassified
// section, all without a second edit to remember.
var licenseSentinels = map[string]bool{unclassifiedLicense: true, oversizeLicense: true}

// Filter compiles the policy into its accept/reject rule once, so a caller
// ruling on hundreds of rows does not rebuild the lookup tables per row.
// [LAW:parse-dont-validate] the returned value is the policy already
// interpreted; nothing downstream re-reads AllowedLicenses or ModuleExceptions.
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
		b.WriteString("Resolve by removing the dependency, or — if the license is genuinely permissive and something lit ships now carries it — adding it to allowed_licenses in tools/licenses/policy.json. A module reported as \"Unknown\" carries neither route: nobody can say what its license permits, so it must go.")
		return fmt.Errorf("%s", b.String())
	}
	// The green line says what the gate proved, and it must not advertise the
	// route the red line above stopped offering: this is the string a release
	// run prints into a log an auditor reads, and it is the only place that
	// audience hears from the gate at all. It names the exception count rather
	// than the word "excepted" so that a reader learns the array is empty from
	// the gate's own output — the guarantee FORKS.md says a green -check
	// carries — instead of having to go and check.
	fmt.Fprintf(stdout, "license policy OK: all %d components are under one of the %d allowlisted licenses, with %d module exceptions in force\n",
		len(entries), len(policy.AllowedLicenses), len(policy.ModuleExceptions))
	return nil
}
