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
	var p Policy
	if err := json.Unmarshal(policyJSON, &p); err != nil {
		return nil, fmt.Errorf("parse embedded policy.json: %w", err)
	}
	// [LAW:no-silent-failure] an empty allowlist would make the gate reject
	// every module — almost certainly a broken/truncated policy file, not a
	// real intent. Refuse rather than fail the whole tree confusingly.
	if len(p.AllowedLicenses) == 0 {
		return nil, fmt.Errorf("policy.json has no allowed_licenses; refusing to run a gate that would reject every module")
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

// CheckPolicy is the accept/reject predicate over the whole inventory: a module
// passes iff its classified license is in AllowedLicenses OR its (module,
// license) pair has a ModuleException. Everything else is a violation. Pure and
// order-deterministic (violations follow entries' order). [LAW:types-are-the-program]
// the two accept shapes are enumerated here, so the reject case is exactly
// everything that is neither — no module, including an Unknown-classified one,
// can slip through without matching one of them.
func CheckPolicy(entries []Entry, p *Policy) []Violation {
	allowed := make(map[string]bool, len(p.AllowedLicenses))
	for _, name := range p.AllowedLicenses {
		allowed[name] = true
	}
	excepted := make(map[exKey]bool, len(p.ModuleExceptions))
	for _, e := range p.ModuleExceptions {
		excepted[exKey{e.Module, e.License}] = true
	}

	var violations []Violation
	for _, e := range entries {
		if allowed[e.LicenseName] {
			continue
		}
		if excepted[exKey{e.Module.Path, e.LicenseName}] {
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
	fmt.Fprintf(stdout, "license policy OK: all %d linked modules are under an allowlisted or explicitly excepted license\n", len(entries))
	return nil
}
