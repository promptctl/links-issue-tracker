package main

import (
	_ "embed"
	"encoding/json"
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
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse embedded policy.json: %w", err)
	}
	// [LAW:no-silent-failure] an empty allowlist would make the gate reject
	// every module — almost certainly a broken/truncated policy file, not a
	// real intent. Refuse rather than fail the whole tree confusingly.
	if len(p.AllowedLicenses) == 0 {
		return nil, fmt.Errorf("policy.json has no allowed_licenses; refusing to run a gate that would reject every module")
	}
	// [LAW:parse-dont-validate] An exception without a module, license, and
	// human-verified reason is the undocumented excuse this policy exists to
	// prevent; the gate must never run against one. Enforced here at the one
	// parse point, so the rule holds however the committed list changes —
	// today's list is empty and a test pins that, but the pin is a policy
	// stance, not this invariant's enforcer.
	for _, e := range p.ModuleExceptions {
		if e.Module == "" || e.License == "" || e.Reason == "" {
			return nil, fmt.Errorf("policy.json module_exception %+v is missing module, license, or reason; every exception must be complete and human-verified", e)
		}
	}
	return &p, nil
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

// Filter compiles the policy into its accept/reject rule once, so a caller
// ruling on hundreds of rows does not rebuild the lookup tables per row.
// [LAW:parse-dont-validate] the returned value is the policy already
// interpreted; nothing downstream re-reads AllowedLicenses or ModuleExceptions.
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
func (f LicenseFilter) Allows(license string) bool { return f.allowed[license] }

// Permits reports whether license is acceptable as module's own license grant:
// it is in AllowedLicenses, or the (module, license) pair carries a
// ModuleException. [LAW:types-are-the-program] the two accept shapes are
// enumerated here, so the reject case is exactly everything that is neither —
// no module, including an Unknown-classified one, can slip through without
// matching one of them.
func (f LicenseFilter) Permits(module, license string) bool {
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
		b.WriteString("Resolve by adding the license to allowed_licenses in tools/licenses/policy.json (if it is permissive) or recording a documented per-module exception there.")
		return fmt.Errorf("%s", b.String())
	}
	fmt.Fprintf(stdout, "license policy OK: all %d components are under an allowlisted or explicitly excepted license\n", len(entries))
	return nil
}
