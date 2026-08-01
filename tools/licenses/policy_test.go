package main

import "testing"

// TestLoadPolicyEmbedded confirms the committed policy.json parses and carries
// the shape the gate depends on: a non-empty allowlist and the two known
// exceptions. If the file is edited into malformed JSON or its allowlist is
// emptied, the gate breaks — this catches that at test time.
func TestLoadPolicyEmbedded(t *testing.T) {
	p, err := LoadPolicy()
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if len(p.AllowedLicenses) == 0 {
		t.Fatal("embedded policy has no allowed_licenses")
	}
	// The two documented exceptions must carry a reason — an undocumented
	// exception is the thing this whole policy exists to prevent.
	if len(p.ModuleExceptions) == 0 {
		t.Fatal("embedded policy has no module_exceptions (expected fslock + buzhash)")
	}
	for _, e := range p.ModuleExceptions {
		if e.Module == "" || e.License == "" || e.Reason == "" {
			t.Errorf("exception %+v is missing module, license, or reason", e)
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
			{Module: "example.com/unclassifiable", License: "Unknown", Reason: "human-verified permissive"},
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
		v := CheckPolicy([]Entry{entry("example.com/mystery", "Unknown")}, policy)
		if len(v) != 1 {
			t.Errorf("unexcepted Unknown should violate, got %+v", v)
		}
	})

	t.Run("Unknown with an exception passes", func(t *testing.T) {
		if v := CheckPolicy([]Entry{entry("example.com/unclassifiable", "Unknown")}, policy); len(v) != 0 {
			t.Errorf("excepted Unknown should pass, got %+v", v)
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

// TestGateRejectsADeniedLicense is this ticket's acceptance #2 — "introducing a
// module under a denied license makes the check fail" — as a deterministic test:
// splice a synthetic GPL-3.0 module into the real inventory and confirm the gate
// flags exactly it. Proves the gate actually rejects, without adding a real
// copyleft dependency to go.mod.
func TestGateRejectsADeniedLicense(t *testing.T) {
	entries, err := buildEntries(litPkg)
	if err != nil {
		t.Fatalf("buildEntries(%s): %v", litPkg, err)
	}
	policy, err := LoadPolicy()
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	poisoned := append(entries, Entry{
		Module:      Module{Path: "example.com/evil-gpl", Version: "v1.2.3"},
		LicenseName: "GPL-3.0",
	})
	violations := CheckPolicy(poisoned, policy)
	if len(violations) != 1 {
		t.Fatalf("want exactly 1 violation (the injected GPL module), got %d: %+v", len(violations), violations)
	}
	if violations[0].Module != "example.com/evil-gpl" || violations[0].License != "GPL-3.0" {
		t.Errorf("violation = %+v, want the injected example.com/evil-gpl GPL-3.0", violations[0])
	}
}
