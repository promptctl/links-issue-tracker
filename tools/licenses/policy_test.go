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
		t.Fatalf("%d module(s) violate the license policy; allowlist the license in policy.json if permissive, or record a documented exception", len(violations))
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
func TestParsePolicyRefusesASentinelLicense(t *testing.T) {
	for _, sentinel := range []string{unclassifiedLicense, oversizeLicense} {
		allowlisted := `{"allowed_licenses":["MIT","` + sentinel + `"],"module_exceptions":[]}`
		if _, err := parsePolicy([]byte(allowlisted)); err == nil {
			t.Errorf("parsePolicy allowlisted the sentinel %q; it names no grant and cannot be permitted", sentinel)
		}
		excepted := `{"allowed_licenses":["MIT"],"module_exceptions":[{"module":"example.com/m","license":"` + sentinel + `","reason":"human-verified"}]}`
		if _, err := parsePolicy([]byte(excepted)); err == nil {
			t.Errorf("parsePolicy accepted an exception for the sentinel %q; there was no license text to have verified", sentinel)
		}
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
			{"parentheses exist to group an OR", []string{"MIT", "Apache-2.0", "(MIT AND Apache-2.0)"}},
			{"an AND-arm that is not itself allowlisted is a grant nobody vetted", []string{"MIT", "MIT AND GPL-2.0-only"}},
			{"a WITH does not exempt the arm's base identifier from the rule", []string{"MIT", "MIT AND Apache-2.0 WITH LLVM-exception"}},
			{"an arm the parser cannot decompose is one the gate cannot rule on", []string{"MIT", "Apache-2.0", "MIT AND Apache-2.0 LLVM-exception"}},
			{"a dangling operator is not an expression", []string{"MIT", "MIT AND"}},
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
