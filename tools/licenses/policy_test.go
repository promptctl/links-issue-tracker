package main

import (
	"strings"
	"testing"
)

// TestLoadPolicyEmbedded confirms the committed policy.json parses and carries
// the shape the gate depends on: a non-empty allowlist and an EMPTY exception
// list — links-licensing-c0ce.4 deleted the last one (fslock) when the fork
// stopped importing it, and the licensing epic's destination is that none ever
// returns. A reappearing exception is a policy regression, not a formality:
// it is a row an auditor stops on. If one is ever genuinely unavoidable, it
// must carry module, license, and a human-verified reason, and this test must
// be loosened deliberately in the same change.
func TestLoadPolicyEmbedded(t *testing.T) {
	p, err := LoadPolicy()
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if len(p.AllowedLicenses) == 0 {
		t.Fatal("embedded policy has no allowed_licenses")
	}
	if len(p.ModuleExceptions) != 0 {
		t.Fatalf("embedded policy carries %d module_exceptions, want none — the licensing epic emptied this list and the gate's promise is that it stays empty: %+v",
			len(p.ModuleExceptions), p.ModuleExceptions)
	}
}

// TestParsePolicyRejectsIncompleteException pins the parse boundary's rule:
// an exception missing its module, license, or human-verified reason never
// becomes a Policy the gate can run against. This enforcement lives in
// parsePolicy, independent of TestLoadPolicyEmbedded's emptiness pin — a
// future change that legitimately reintroduces an exception loosens that pin,
// and this rule must hold without anyone remembering to re-add it.
func TestParsePolicyRejectsIncompleteException(t *testing.T) {
	for _, missing := range []string{
		`{"allowed_licenses":["MIT"],"module_exceptions":[{"module":"","license":"LGPL-3.0","reason":"r"}]}`,
		`{"allowed_licenses":["MIT"],"module_exceptions":[{"module":"example.com/m","license":"","reason":"r"}]}`,
		`{"allowed_licenses":["MIT"],"module_exceptions":[{"module":"example.com/m","license":"LGPL-3.0","reason":""}]}`,
		`{"allowed_licenses":["MIT"],"module_exceptions":[{"module":"example.com/m","license":"LGPL-3.0","reason":"  "}]}`,
		`{"allowed_licenses":["MIT"],"module_exceptions":[{"module":" example.com/m ","license":"LGPL-3.0","reason":"r"}]}`,
	} {
		if _, err := parsePolicy([]byte(missing)); err == nil {
			t.Errorf("parsePolicy accepted an incomplete exception: %s", missing)
		}
	}
	complete := `{"allowed_licenses":["MIT"],"module_exceptions":[{"module":"example.com/m","license":"LGPL-3.0","reason":"human-verified"}]}`
	if _, err := parsePolicy([]byte(complete)); err != nil {
		t.Errorf("parsePolicy rejected a complete exception: %v", err)
	}
}

// TestParsePolicyRejectsUnknownKeys pins that a misspelled key in the
// hand-edited policy fails the parse instead of silently unmarshaling into
// an absent field — a typo'd module_exceptions would otherwise read as an
// empty exception list while the file still carries a row.
func TestParsePolicyRejectsUnknownKeys(t *testing.T) {
	misspelled := `{"allowed_licenses":["MIT"],"module_exception":[{"module":"example.com/m","license":"LGPL-3.0","reason":"r"}]}`
	if _, err := parsePolicy([]byte(misspelled)); err == nil {
		t.Error("parsePolicy accepted a policy with an unknown (misspelled) key")
	}
}

// TestParsePolicyRejectsDuplicateKeys pins that a key repeated within one
// object fails the parse: encoding/json keeps the last value, so a bad merge
// leaving two module_exceptions keys would otherwise silently drop the row
// the committed text still shows.
func TestParsePolicyRejectsDuplicateKeys(t *testing.T) {
	dup := `{"allowed_licenses":["MIT"],"module_exceptions":[{"module":"example.com/m","license":"LGPL-3.0","reason":"r"}],"module_exceptions":[]}`
	if _, err := parsePolicy([]byte(dup)); err == nil {
		t.Error("parsePolicy accepted a policy with a duplicated module_exceptions key")
	}
	nested := `{"allowed_licenses":["MIT"],"module_exceptions":[{"module":"example.com/m","module":"example.com/n","license":"LGPL-3.0","reason":"r"}]}`
	if _, err := parsePolicy([]byte(nested)); err == nil {
		t.Error("parsePolicy accepted a policy with a duplicated key inside an exception object")
	}
}

// TestParsePolicyRejectsMalformedAllowlistEntry pins the allowlist under the
// same rule as exceptions: the committed text is the exact string the gate
// matches, so a blank or padded entry never becomes a Policy.
func TestParsePolicyRejectsMalformedAllowlistEntry(t *testing.T) {
	for _, bad := range []string{
		`{"allowed_licenses":["MIT",""],"module_exceptions":[]}`,
		`{"allowed_licenses":[" MIT "],"module_exceptions":[]}`,
	} {
		if _, err := parsePolicy([]byte(bad)); err == nil {
			t.Errorf("parsePolicy accepted a malformed allowlist entry: %s", bad)
		}
	}
}

// TestParsePolicyRejectsTrailingContent pins that the parse consumes the
// whole file: a concatenated second document (a bad merge, a paste artifact)
// would otherwise be silently dropped and the gate would run against only
// the first value.
func TestParsePolicyRejectsTrailingContent(t *testing.T) {
	for _, trailing := range []string{
		`{"allowed_licenses":["MIT"],"module_exceptions":[]}{"module_exceptions":[{"module":"example.com/m","license":"LGPL-3.0","reason":"r"}]}`,
		`{"allowed_licenses":["MIT"],"module_exceptions":[]} garbage`,
	} {
		if _, err := parsePolicy([]byte(trailing)); err == nil {
			t.Errorf("parsePolicy accepted trailing content: %s", trailing)
		}
	}
}

// TestCheckPolicyAcceptReject is the accept/reject table for the gate predicate.
// The two accept shapes (allowlisted license; matching (module,license)
// exception) and everything that is neither a reject are enumerated here so the
// predicate can't silently widen.
func TestCheckPolicyAcceptReject(t *testing.T) {
	policy := &Policy{
		AllowedLicenses: []string{"MIT", "Apache-2.0"},
		ModuleExceptions: []ModuleException{
			{Module: "example.com/lgpl-with-exception", License: "LGPL-3.0", Reason: "static-linking exception"},
			{Module: "example.com/unclassifiable", License: unclassifiedLicense, Reason: "a reason nobody could have had"},
		},
	}
	entry := func(path, license string) Entry {
		return Entry{Module: Module{Path: path, Version: "v1.0.0"}, LicenseName: license}
	}

	t.Run("allowlisted license passes", func(t *testing.T) {
		if v := CheckPolicy([]Entry{entry("example.com/a", "MIT")}, policy); len(v) != 0 {
			t.Errorf("MIT should pass, got violations %+v", v)
		}
	})

	t.Run("denied license is a violation", func(t *testing.T) {
		v := CheckPolicy([]Entry{entry("example.com/bad", "GPL-3.0")}, policy)
		if len(v) != 1 || v[0].Module != "example.com/bad" || v[0].License != "GPL-3.0" {
			t.Errorf("GPL-3.0 should violate, got %+v", v)
		}
	})

	t.Run("matching (module,license) exception passes", func(t *testing.T) {
		if v := CheckPolicy([]Entry{entry("example.com/lgpl-with-exception", "LGPL-3.0")}, policy); len(v) != 0 {
			t.Errorf("excepted module should pass, got %+v", v)
		}
	})

	t.Run("exception does not apply to a different module", func(t *testing.T) {
		// Another module under the same LGPL-3.0 must NOT ride the exception —
		// the exception is scoped to one module, not the license globally.
		v := CheckPolicy([]Entry{entry("example.com/other", "LGPL-3.0")}, policy)
		if len(v) != 1 {
			t.Errorf("LGPL-3.0 on a non-excepted module should violate, got %+v", v)
		}
	})

	t.Run("exception does not apply when the license reclassifies", func(t *testing.T) {
		// The excepted module under a DIFFERENT license (e.g. an upstream
		// relicense to plain GPL) loses the exception and violates loudly.
		v := CheckPolicy([]Entry{entry("example.com/lgpl-with-exception", "GPL-3.0")}, policy)
		if len(v) != 1 || v[0].License != "GPL-3.0" {
			t.Errorf("reclassified excepted module should violate, got %+v", v)
		}
	})

	t.Run("Unknown without an exception is a violation", func(t *testing.T) {
		v := CheckPolicy([]Entry{entry("example.com/mystery", unclassifiedLicense)}, policy)
		if len(v) != 1 {
			t.Errorf("unexcepted Unknown should violate, got %+v", v)
		}
	})

	t.Run("Unknown with an exception is STILL a violation", func(t *testing.T) {
		// The policy above grants example.com/unclassifiable a complete,
		// reasoned exception for the sentinel and it buys nothing:
		// links-licensing-c0ce.9 made an unreadable license a hard failure
		// with no route around it, because an exception records a human
		// having read a grant and there was no grant to read. The exception
		// is left in the fixture deliberately — a test that simply omitted it
		// would pass just as well against a filter that still honoured one.
		v := CheckPolicy([]Entry{entry("example.com/unclassifiable", unclassifiedLicense)}, policy)
		if len(v) != 1 {
			t.Errorf("an excepted Unknown must still violate — the sentinel has no exception path, got %+v", v)
		}
	})
}

// TestDependencyLicensesArePermitted IS the license-policy gate (this ticket's
// acceptance #1): it runs the real classifier over the real linked module set
// and asserts zero policy violations. It runs on every PR and master push via
// ci.yml's `go test ./...`, so a dependency bump that pulls in a non-allowlisted
// license fails the build here. [LAW:verifiable-goals]
func TestDependencyLicensesArePermitted(t *testing.T) {
	entries, err := buildEntries(litPkg)
	if err != nil {
		t.Fatalf("buildEntries(%s): %v", litPkg, err)
	}
	policy, err := LoadPolicy()
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	violations := CheckPolicy(entries, policy)
	if len(violations) > 0 {
		for _, v := range violations {
			t.Errorf("non-allowlisted license: %s@%s is %s", v.Module, v.Version, v.License)
		}
		// Says the same thing runCheck's failure says, because this is the
		// message a developer actually meets: `go test ./...` runs on every
		// PR and release-validate's -check does not. Telling them here to
		// "record a documented exception" would send them to write one that
		// parsePolicy then refuses on its own separate grounds — a second,
		// unrelated-looking failure produced by obeying the first message.
		t.Fatalf("%d module(s) violate the license policy; remove the dependency, or add its license to allowed_licenses in tools/licenses/policy.json if something lit ships now carries it. module_exceptions is empty by design and an \"Unknown\" row has neither route", len(violations))
	}
}

// TestGateRejectsADeniedLicense splices one synthetic module at a time into the
// REAL inventory and confirms the gate flags exactly it — proof that the gate
// rejects, without adding a real copyleft dependency to go.mod.
//
// The table is the licensing epic's destination stated as failures, and every
// row is a license that was genuinely reachable here: GPL-3.0 arrived through a
// dolt test file (links-licensing-c0ce.7), LGPL-3.0 was fslock's and rode a
// documented exception (.4), MPL-2.0 sat in allowed_licenses to cover golang-lru
// and go-sql-driver/mysql (.5, .2), and the Unknown sentinel was kch42/buzhash's
// unclassifiable WTFPL variant, also on an exception (.6). Three of the four
// therefore USED to pass this gate. links-licensing-c0ce.9 closed both routes —
// the allowlist entry is gone and the sentinel has no exception path — so a row
// that stops failing means one of them was re-opened. [LAW:verifiable-goals]
func TestGateRejectsADeniedLicense(t *testing.T) {
	entries, err := buildEntries(litPkg)
	if err != nil {
		t.Fatalf("buildEntries(%s): %v", litPkg, err)
	}
	policy, err := LoadPolicy()
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	for _, tc := range []struct{ module, license string }{
		{"example.com/evil-gpl", "GPL-3.0"},
		{"example.com/evil-lgpl", "LGPL-3.0"},
		{"example.com/evil-mpl", "MPL-2.0"},
		{"example.com/mystery", unclassifiedLicense},
	} {
		t.Run(tc.license, func(t *testing.T) {
			// Copy rather than append in place: append would reuse entries'
			// backing array across subtests and each row would overwrite the
			// last one's injected module.
			poisoned := append(append([]Entry(nil), entries...), Entry{
				Module:      Module{Path: tc.module, Version: "v1.2.3"},
				LicenseName: tc.license,
			})
			violations := CheckPolicy(poisoned, policy)
			if len(violations) != 1 {
				t.Fatalf("want exactly 1 violation (the injected %s module), got %d: %+v", tc.license, len(violations), violations)
			}
			if violations[0].Module != tc.module || violations[0].License != tc.license {
				t.Errorf("violation = %+v, want the injected %s %s", violations[0], tc.module, tc.license)
			}
		})
	}
}

// TestParsePolicyRefusesASentinelLicense pins that the committed file can never
// STATE the rule links-licensing-c0ce.9 removed. Filter already drops a sentinel
// from both lookup tables, so a policy naming one would be inert — and an inert
// rule in a hand-edited compliance file is worse than a rejected one, because
// the next reader believes it. [LAW:no-silent-failure]
// It asserts the DIAGNOSTIC, not merely that some error came back, and that is
// load-bearing rather than fussy. oversizeLicense is literally "Skipped
// (oversize)", so a bare error check on that row is satisfied by the
// parenthesis rule and pins nothing at all about the sentinel — delete the
// sentinel guard and the row stays green while the file's diagnostic silently
// becomes "is parenthesized; SPDX parentheses group an OR", which is nonsense
// advice for this input. Naming the phrase makes the row fail for the reason
// it exists.
func TestParsePolicyRefusesASentinelLicense(t *testing.T) {
	const want = "not a license but this tool's marker for having no verdict"
	for _, sentinel := range []string{unclassifiedLicense, oversizeLicense} {
		allowlisted := `{"allowed_licenses":["MIT","` + sentinel + `"],"module_exceptions":[]}`
		_, err := parsePolicy([]byte(allowlisted))
		if err == nil {
			t.Errorf("parsePolicy allowlisted the sentinel %q; it names no grant and cannot be permitted", sentinel)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("allowlisted sentinel %q was refused for the wrong reason: %v", sentinel, err)
		}

		excepted := `{"allowed_licenses":["MIT"],"module_exceptions":[{"module":"example.com/m","license":"` + sentinel + `","reason":"human-verified"}]}`
		_, err = parsePolicy([]byte(excepted))
		if err == nil {
			t.Errorf("parsePolicy accepted an exception for the sentinel %q; there was no license text to have verified", sentinel)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("excepted sentinel %q was refused for the wrong reason: %v", sentinel, err)
		}
	}
}

// TestParsePolicyExpressionRulesReachExceptions pins that the expression rules
// are about this FILE and not about the allowlist alone. They had a door beside
// them until links-licensing-c0ce.9's review: parsePolicy checked allowlist
// entries and walked straight past module_exceptions, so "BSD-3-Clause OR
// GPL-2.0-only" — zstd's own un-elected upstream grant, the exact un-made
// election the OR ban exists to refuse — parsed clean and became a live key in
// Filter's exception table, while the identical string one field away was
// rejected.
//
// The AND-arm vetting rule deliberately does NOT reach here, and the last case
// pins that too: an exception is not a permission granted to a license, it is
// one module's grant that a human read, so requiring its arms to be
// allowlisted would contradict the only reason exceptions exist.
func TestParsePolicyExpressionRulesReachExceptions(t *testing.T) {
	exception := func(license string) []byte {
		return []byte(`{"allowed_licenses":["MIT"],"module_exceptions":[{"module":"example.com/m","license":"` + license + `","reason":"human-verified"}]}`)
	}
	for _, tc := range []struct {
		license string
		why     string
	}{
		{"BSD-3-Clause OR GPL-2.0-only", "an un-elected dual license is refused whichever field it sits in"},
		{"MIT or GPL-2.0-only", "a lowercase operator does not smuggle one past the OR ban"},
		{"(MIT AND Apache-2.0)", "parentheses group an OR and have no place in either field"},
		{"MIT  AND  Apache-2.0", "collapsed whitespace would certify a string the gate can never match"},
		{"Apache-2.0 LLVM-exception", "an arm the parser cannot decompose is one the gate cannot rule on"},
	} {
		if _, err := parsePolicy(exception(tc.license)); err == nil {
			t.Errorf("parsePolicy accepted the exception license %q — %s", tc.license, tc.why)
		}
	}
	if _, err := parsePolicy(exception("LGPL-3.0 AND Apache-2.0")); err != nil {
		t.Errorf("parsePolicy refused a well-formed exception whose arms are not allowlisted: %v — an exception's whole purpose is to name a license the allowlist does not carry", err)
	}
}

// TestParsePolicyExpressionRules pins the two rules that make an allowlisted
// SPDX EXPRESSION mean what it appears to mean. They matter because matching is
// by exact string and nothing downstream parses an entry, so an expression
// otherwise allowlists a whole combination while the gate vets none of its
// arms — the reason "MIT AND Apache-2.0 WITH LLVM-exception" is safe is that a
// human checked MIT and Apache-2.0 separately, and before this parse existed
// nothing at all distinguished that entry from "MIT OR GPL-2.0-only".
func TestParsePolicyExpressionRules(t *testing.T) {
	policy := func(entries ...string) []byte {
		quoted := make([]string, len(entries))
		for i, e := range entries {
			quoted[i] = `"` + e + `"`
		}
		return []byte(`{"allowed_licenses":[` + strings.Join(quoted, ",") + `],"module_exceptions":[]}`)
	}

	t.Run("refused", func(t *testing.T) {
		for _, tc := range []struct {
			why     string
			entries []string
		}{
			{"an OR means a dual license reached policy with its election unmade", []string{"MIT", "MIT OR GPL-2.0-only"}},
			{"an OR is refused even when both arms are separately allowlisted", []string{"MIT", "Apache-2.0", "MIT OR Apache-2.0"}},
			{"an AND-arm that is not itself allowlisted is a grant nobody vetted", []string{"MIT", "MIT AND GPL-2.0-only"}},
			{"a WITH does not exempt the arm's base identifier from the rule", []string{"MIT", "MIT AND Apache-2.0 WITH LLVM-exception"}},
			{"an arm the parser cannot decompose is one the gate cannot rule on", []string{"MIT", "Apache-2.0", "MIT AND Apache-2.0 LLVM-exception"}},
			{"a dangling operator is not an expression", []string{"MIT", "MIT AND"}},

			// Each row below fails ONLY if its own rule is present. The paren
			// rule had no such row until this review: "(MIT AND Apache-2.0)"
			// decomposes to the arms "(MIT" and "Apache-2.0)", neither of them
			// allowlisted, so it was refused by the AND-arm rule with or
			// without the parenthesis guard — deleting the guard left the
			// whole package green while a parenthesized entry could reach the
			// committed compliance file. A single-arm "(MIT)" is refusable by
			// nothing else. [LAW:verifiable-goals]
			{"parentheses group an OR and are refused even with one arm", []string{"MIT", "(MIT)"}},
			{"a lowercase operator is not an identifier token", []string{"MIT", "MIT or GPL-2.0-only"}},
			// This row is the only one that isolates the operator-case rule.
			// Every other lowercase spelling is caught downstream anyway — a
			// non-canonical OR still reaches the OR ban, and a non-canonical
			// WITH still fails the arm shape — so with both arms allowlisted
			// and the operator an AND, the case rule is the last thing left
			// standing between this entry and acceptance. Mutation-checked:
			// remove that rule and this row, and only this row, goes green.
			{"a mixed-case AND is malformed SPDX even when both arms are allowlisted", []string{"MIT", "Apache-2.0", "MIT And Apache-2.0"}},
			{"an arm shape is checked even when the entry is not a combination", []string{"Apache-2.0 LLVM-exception"}},
			{"a lone WITH is not an identifier", []string{"MIT", "Apache-2.0 WITH"}},
			{"a missing operator between two identifiers is not an expression", []string{"MIT", "Apache-2.0", "MIT Apache-2.0"}},
			{"whitespace is matched byte for byte, so a doubled space is a different string", []string{"MIT", "Apache-2.0", "MIT AND  Apache-2.0 WITH LLVM-exception"}},
		} {
			if _, err := parsePolicy(policy(tc.entries...)); err == nil {
				t.Errorf("parsePolicy accepted %v — %s", tc.entries, tc.why)
			}
		}
	})

	t.Run("accepted", func(t *testing.T) {
		for _, tc := range []struct {
			why     string
			entries []string
		}{
			{"the committed expression, with both arms independently present", []string{"MIT", "Apache-2.0", "MIT AND Apache-2.0 WITH LLVM-exception"}},
			{"a bare identifier has no arms to check", []string{"MIT"}},
			{"a single arm IS the vetted entry, so a lone WITH needs no base", []string{"Apache-2.0 WITH LLVM-exception"}},
			{"an identifier that merely contains an operator's letters is one token", []string{"XOR-1.0", "WITHERED-2.0"}},
		} {
			if _, err := parsePolicy(policy(tc.entries...)); err != nil {
				t.Errorf("parsePolicy rejected %v (%s): %v", tc.entries, tc.why, err)
			}
		}
	})
}
