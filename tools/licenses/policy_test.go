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

// TestParsePolicyRejectsCaseVariantKeys pins the hole round 2 of review found
// in the two guards above, which between them looked airtight and were not.
//
// encoding/json resolves an object key to a struct field case-INSENSITIVELY as
// a fallback, so "ALLOWED_LICENSES" is not an unknown field and
// DisallowUnknownFields never fires on it; rejectDuplicateKeys compared raw key
// text, so it saw two different strings and no duplicate. A committed
// policy.json could therefore show a reader `"module_exceptions": []` while the
// gate ran against a `"MODULE_EXCEPTIONS"` holding a live exception — the exact
// committed-text-versus-gate's-view split those guards exist to refuse,
// arriving through the one spelling neither of them checked.
func TestParsePolicyRejectsCaseVariantKeys(t *testing.T) {
	for _, tc := range []struct{ why, doc string }{
		{
			"a case-variant allowed_licenses would decide what the gate permits",
			`{"allowed_licenses":["MIT"],"ALLOWED_LICENSES":["GPL-3.0"],"module_exceptions":[]}`,
		},
		{
			"a case-variant module_exceptions would carry a live exception past an empty-looking one",
			`{"allowed_licenses":["MIT"],"module_exceptions":[],"MODULE_EXCEPTIONS":[{"module":"example.com/evil","license":"GPL-3.0","reason":"reads fine"}]}`,
		},
	} {
		if _, err := parsePolicy([]byte(tc.doc)); err == nil {
			t.Errorf("parsePolicy accepted a case-variant duplicate key — %s: %s", tc.why, tc.doc)
		}
	}
}

// TestParsePolicyRejectsMalformedAllowlistEntry pins the allowlist under the
// same rule as exceptions: the committed text is the exact string the gate
// matches, so a blank or padded entry never becomes a Policy.
//
// The diagnostic is asserted, not merely the error: parseLicenseExpression's
// character and whitespace rules refuse both of these inputs on their own, so
// a bare error check leaves this rule deletable with the suite green while a
// padded entry starts being answered with advice about wrapped whitespace.
func TestParsePolicyRejectsMalformedAllowlistEntry(t *testing.T) {
	const want = "is blank or carries surrounding whitespace"
	for _, bad := range []string{
		`{"allowed_licenses":["MIT",""],"module_exceptions":[]}`,
		`{"allowed_licenses":[" MIT "],"module_exceptions":[]}`,
	} {
		_, err := parsePolicy([]byte(bad))
		if err == nil {
			t.Errorf("parsePolicy accepted a malformed allowlist entry: %s", bad)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("parsePolicy refused %s for the wrong reason (want %q): %v", bad, want, err)
		}
	}
}

// TestParsePolicyRejectsMalformedModulePath pins the module half of an
// exception under the same rule as the license half. The path is matched byte
// for byte against `go list` output, TrimSpace sees neither an interior
// zero-width space nor a full-width character, and an exception keyed on a path
// nothing can equal excuses nothing while reading as though it does.
func TestParsePolicyRejectsMalformedModulePath(t *testing.T) {
	const want = "not a valid module path"
	// The first four are character defects, and the space and the @ are the two
	// that mattered: the guard originally reused isSPDXRune, whose alphabet
	// PERMITS a space because a space separates the arms of an expression, and
	// additionally whitelisted @ — so it admitted exactly the dead keys its own
	// comment claimed to refuse. A module_exception names a path, never a
	// path@version.
	//
	// The rest are STRUCTURAL, and are the reason the rune filter was replaced
	// by module.CheckPath outright: every one of them is fine character by
	// character and none is a path `go list` can print.
	for _, path := range []string{
		"example.com/m\u200bx", "example.com/（m）", "example.com/m x", "example.com/m@v1.0.0",
		"example.com//m", "example.com/m/", "nodot/m", "example.com/../m", "example.com/.hidden",
	} {
		doc := `{"allowed_licenses":["MIT"],"module_exceptions":[{"module":"` + path + `","license":"LGPL-3.0","reason":"human-verified"}]}`
		_, err := parsePolicy([]byte(doc))
		if err == nil {
			t.Errorf("parsePolicy accepted the module path %q, which nothing `go list` prints can equal", path)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("parsePolicy refused %q for the wrong reason (want %q): %v", path, want, err)
		}
	}
	// A real module path, with every character a path legitimately carries.
	ok := `{"allowed_licenses":["MIT"],"module_exceptions":[{"module":"gopkg.in/yaml.v3","license":"LGPL-3.0","reason":"human-verified"}]}`
	if _, err := parsePolicy([]byte(ok)); err != nil {
		t.Errorf("parsePolicy rejected an ordinary module path: %v", err)
	}
}

// TestParsePolicyRejectsDuplicateException pins that two exception rows for one
// (module, license) pair never load. Filter keys exceptions on exactly that
// pair, so the second collapses onto the first and is invisible to the gate
// while -check's green line still counts it — and two human-verified reasons
// for one grant is a question in its own right.
func TestParsePolicyRejectsDuplicateException(t *testing.T) {
	dup := `{"allowed_licenses":["MIT"],"module_exceptions":[` +
		`{"module":"example.com/m","license":"LGPL-3.0","reason":"first reading"},` +
		`{"module":"example.com/m","license":"LGPL-3.0","reason":"second reading"}]}`
	if _, err := parsePolicy([]byte(dup)); err == nil {
		t.Error("parsePolicy accepted two module_exceptions for one (module, license) pair")
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
	t.Parallel()
	entries := realEntries(t)
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
		// PR and release-validate's -check does not. The old wording pointed
		// at module_exceptions, which policy.json now spends a paragraph
		// asking nobody to reopen — and for the Unknown case it pointed at a
		// route the parse refuses outright, so obeying it produced a second,
		// unrelated-looking failure. For a copyleft license an exception WOULD
		// parse; it is simply the wrong answer, and this message says which
		// answer is right rather than leaving the choice open.
		t.Fatalf("%d module(s) violate the license policy; remove the dependency, or — if the license is genuinely permissive and something lit ships now carries it — add it to allowed_licenses in tools/licenses/policy.json. A copyleft license is refused there by the parse, module_exceptions is empty by design, and an \"Unknown\" row has no route at all", len(violations))
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
// unclassifiable WTFPL variant, also on an exception (.6). Exactly ONE of the
// four passed this gate in the state links-licensing-c0ce.9 changed — MPL-2.0,
// on the allowlist. LGPL-3.0 and the unclassifiable one had each passed
// EARLIER in the epic, on exceptions .4 and .6 deleted along with the
// dependencies behind them. .9 closed what was left: the allowlist entry is
// gone and the sentinel has no exception path.
//
// Be exact about what a row here can detect, rather than claiming the table
// catches every reopening — and note that the answer changed under this test's
// feet. When these rows were written, MPL-2.0 was the sharp one: put it back in
// allowed_licenses and this row goes green with nothing else changing. The
// copyleft veto added in the SAME commit made that edit a load failure too, so
// all four rows are now a second line of defence: each needs a policy.json edit
// that parsePolicy independently refuses, and the refusal surfaces as a failure
// to load rather than as a violation here.
//
// That does not make the table pointless — it is the only thing asserting the
// gate REJECTS rather than merely that the policy is well-formed, and it runs
// against the real inventory — but "a row that stops failing means a route was
// re-opened" was true when written and is not now. [LAW:verifiable-goals]
func TestGateRejectsADeniedLicense(t *testing.T) {
	t.Parallel()
	entries := realEntries(t)
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
// STATE a rule it does not have. The rulings refuse a sentinel whatever the
// lookup tables hold, so a policy naming one would be inert — and an inert rule
// in a hand-edited compliance file is worse than a rejected one, because the
// next reader believes it. [LAW:no-silent-failure]
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

	// Several of these rules refuse overlapping inputs, so "it errored" pins
	// almost nothing: the OR ban and the parenthesis rule are both refusable
	// by the arm-shape and character rules standing behind them, and a row
	// that only checks for a non-nil error stays green when the rule it names
	// is deleted. Round 1 of review found exactly that for the parenthesis
	// rule; round 2 found it for the OR ban, this change's headline. So every
	// row names the phrase its own rule produces, and a mutation that removes
	// the rule changes the diagnostic and fails the row even when something
	// else still refuses the input. [LAW:verifiable-goals]
	t.Run("refused with the right diagnostic", func(t *testing.T) {
		for _, tc := range []struct {
			why     string
			want    string
			entries []string
		}{
			{"an OR means a dual license reached policy with its election unmade", "an election went unmade", []string{"MIT", "MIT OR GPL-2.0-only"}},
			{"an OR is refused even when both arms are separately allowlisted", "an election went unmade", []string{"MIT", "Apache-2.0", "MIT OR Apache-2.0"}},
			{"parentheses group an OR, whatever they contain", "is parenthesized", []string{"MIT", "Apache-2.0", "(MIT AND Apache-2.0)"}},
			{"a single-arm parenthesized entry is refusable by nothing else", "is parenthesized", []string{"MIT", "(MIT)"}},
			{"a bare operator is not a license name", "where an identifier belongs", []string{"MIT", "WITH"}},
			{"a sentinel wearing an exception is still a sentinel", "no verdict", []string{"MIT", unclassifiedLicense + " WITH some-exception"}},
			{"an invisible character parses clean and then never matches", "which no SPDX identifier", []string{"MIT", "MIT​"}},
			{"a full-width parenthesis is not the ASCII one the paren rule sees", "which no SPDX identifier", []string{"MIT", "（MIT）"}},
			{"a duplicated entry makes the printed count a lie", "repeats", []string{"MIT", "MIT"}},
			// The sentinel spellings are ours, so a case variant means the
			// sentinel and nothing else. Accepted as a plain identifier it
			// becomes an entry that can never match a classified license —
			// inert, and therefore a statement to the next reader about what
			// this repository accepts that is not true.
			{"a lower-cased sentinel is still the sentinel", "no verdict", []string{"MIT", "unknown"}},
			// Not a duplicate by byte comparison, which is why the rule folds
			// case: at most one of the two is a name the classifier emits, so
			// the other can never match anything and exists only to inflate a
			// count -check prints as a fact.
			{"two entries differing only in case", "differ only in case", []string{"MIT", "mit"}},
			{"a copyleft license may not be allowlisted at all", "which the classifier types as", []string{"MIT", "GPL-3.0"}},
			// The current SPDX spelling, which the classifier's corpus
			// predates: LicenseType("GPL-3.0-only") is "" and only the
			// deprecated "GPL-3.0" types restricted. Without the spelling
			// normalization this row is accepted outright — and native.go
			// already writes the modern form, so it is the spelling that would
			// actually arrive.
			{"the modern spelling of a copyleft license is the same license", "which the classifier types as", []string{"MIT", "GPL-3.0-only"}},
			// The lookup is exact, and every copyleft family the veto exists
			// to catch is spelled in capitals, so upper-casing the identifier
			// lands it on the corpus spelling. Without that step this row is
			// accepted and the veto is one shift-key from useless.
			{"a lower-cased copyleft identifier is the same license", "which the classifier types as", []string{"MIT", "gpl-3.0"}},
			{"and the or-later form of it", "which the classifier types as", []string{"MIT", "AGPL-3.0-or-later"}},
			// The WITH half of an arm, which is not a formality: Commons-Clause
			// is an SPDX exception that removes the right to sell, so this is a
			// narrowed grant wearing a permissive base. Isolates the exception
			// half of the veto — the BASE half cannot be isolated at all, because
			// a compound's bases must each be present as their own entry and are
			// vetted there first. See checkAllowedLicenses.
			{"an exception can narrow a grant, so it is vetted too", "which the classifier types as", []string{"MIT", "Apache-2.0", "Apache-2.0 WITH Commons-Clause"}},
			{"a sentinel in the exception half is still a sentinel", "no verdict", []string{"MIT", "MIT WITH " + unclassifiedLicense}},
			// Pins the arm-shape rule's `arm[1] == "WITH"` half specifically.
			// Without it a three-token arm is accepted and its MIDDLE token
			// silently discarded, so "MIT AND Apache-2.0" — an expression with
			// its operator mistyped — would read as the arm "MIT" plus a
			// dropped "AND". Mutation-checked: delete that half of the
			// condition and this row, and only this row, goes green.
			{"a three-token arm whose middle token is not WITH is not an arm", "neither an SPDX identifier", []string{"MIT", "Apache-2.0", "Apache-2.0 AND2 LLVM-exception"}},
			// WITH is the only operator that can reach the exception slot —
			// AND and OR split the expression into arms before the arm shape
			// is read, so "MIT WITH AND" is refused as an undecomposable arm
			// rather than as an operator in the wrong place.
			{"an operator in the exception half is not an exception", "where an identifier belongs", []string{"MIT", "MIT WITH WITH"}},
			{"a non-breaking space is named as the character it is", "which no SPDX identifier", []string{"MIT", "MIT AND\u00a0Apache-2.0"}},
		} {
			_, err := parsePolicy(policy(tc.entries...))
			if err == nil {
				t.Errorf("parsePolicy accepted %v — %s", tc.entries, tc.why)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parsePolicy refused %v for the wrong reason (want %q) — %s: %v", tc.entries, tc.want, tc.why, err)
			}
		}
	})

	t.Run("refused", func(t *testing.T) {
		for _, tc := range []struct {
			why     string
			entries []string
		}{
			{"an AND-arm that is not itself allowlisted is a grant nobody vetted", []string{"MIT", "MIT AND Unicode-3.0"}},
			{"a WITH-arm is not satisfied by its bare base, because an exception can narrow", []string{"MIT", "Apache-2.0", "MIT AND Apache-2.0 WITH LLVM-exception"}},
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
			{"the committed expression, with each arm present exactly as the expression spells it", []string{"MIT", "Apache-2.0", "Apache-2.0 WITH LLVM-exception", "MIT AND Apache-2.0 WITH LLVM-exception"}},
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
