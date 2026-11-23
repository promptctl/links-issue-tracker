package main

import (
	"strings"
	"testing"
)

func TestAClassParsesToItsTouchesDispatchOrSpawn(t *testing.T) {
	for spec, want := range map[string]Class{
		"none":                Class{},
		"fs":                  {Touches: Touches(0).With(FS)},
		"store,git":           {Touches: Touches(0).With(Store).With(Git)},
		"dispatching":         {Dispatching: true},
		"net,dispatching":     {Touches: Touches(0).With(Net), Dispatching: true},
		"proc,clock":          {Touches: Touches(0).With(Proc).With(Clock)},
		"spawn:0":             {Spawn: &Spawn{Arg: 0}},
		"spawn:2":             {Spawn: &Spawn{Arg: 2}},
		"spawn:field":         {Spawn: &Spawn{Field: true}},
		"store,git,fs,clock":  {Touches: Touches(0).With(Store).With(Git).With(FS).With(Clock)},
		"git,dispatching,net": {Touches: Touches(0).With(Git).With(Net), Dispatching: true},
	} {
		got, err := parseClass(spec)
		if err != nil {
			t.Errorf("parseClass(%q) error = %v", spec, err)
			continue
		}
		if got.Touches != want.Touches || got.Dispatching != want.Dispatching || (got.Spawn == nil) != (want.Spawn == nil) || (got.Spawn != nil && *got.Spawn != *want.Spawn) {
			t.Errorf("parseClass(%q) = %+v, want %+v", spec, got, want)
		}
	}
}

func TestAClassNamingNoTouchRefuses(t *testing.T) {
	// cli is a touch, but not one an outside function makes: a CLI run is
	// decided by what a spawn runs.
	for _, spec := range []string{"", "disk", "fs,", "cli", "spawn:", "spawn:-1", "spawn:x"} {
		if _, err := parseClass(spec); err == nil {
			t.Errorf("parseClass(%q) accepted it", spec)
		}
	}
}

func TestAClassificationFileRefusesWhatItCannotParse(t *testing.T) {
	for name, data := range map[string]string{
		"an unknown field":   `{"packages": {}, "extra": 1}`,
		"a bad default":      `{"packages": {"os": {"default": "disk"}}}`,
		"a bad function":     `{"packages": {"os": {"default": "fs", "functions": {"Getenv": "nothing"}}}}`,
		"a cli without a .":  `{"cli": ["nowhere"], "packages": {}}`,
		"not a json object":  `[]`,
		"an unknown package": `{"packages": {"os": {"default": "fs", "notes": "x"}}}`,
	} {
		if _, err := parseClassification([]byte(data)); err == nil || !strings.HasPrefix(err.Error(), "classification file: ") {
			t.Errorf("%s: parseClassification error = %v, want a classification file error", name, err)
		}
	}
}

func TestLookupTakesTheMostSpecificEntry(t *testing.T) {
	class, err := parseClassification([]byte(`{
		"git": ["git"],
		"packages": {
			"os": {"default": "fs", "functions": {"Getenv": "none", "Process": "proc", "Process.Pid": "none"}},
			"example.com/dolt/...": {"default": "store,git"},
			"example.com/dolt/utils/...": {"default": "fs"},
			"example.com/dolt/utils/hash": {"default": "none"}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ pkg, key, want string }{
		{"os", "ReadFile", "fs"},                      // the package default
		{"os", "Getenv", "none"},                      // its own entry
		{"os", "Process.Kill", "proc"},                // its type's entry
		{"os", "Process.Pid", "none"},                 // its own entry over its type's
		{"example.com/dolt", "Open", "store,git"},     // a pattern covers its own path
		{"example.com/dolt/sql", "Open", "store,git"}, // and what is under it
		{"example.com/dolt/utils/fs", "Open", "fs"},   // the longest pattern wins
		{"example.com/dolt/utils/hash", "Of", "none"}, // an exact entry over any pattern
	} {
		cl, ok := class.Lookup(tc.pkg, tc.key)
		got := "none"
		if cl.Touches != 0 {
			got = cl.Touches.String()
		}
		if !ok || got != tc.want {
			t.Errorf("Lookup(%s, %s) = %s (found %v), want %s", tc.pkg, tc.key, got, ok, tc.want)
		}
	}
	for _, pkg := range []string{"strings", "example.com/doltish", "example.com"} {
		if _, ok := class.Lookup(pkg, "F"); ok {
			t.Errorf("Lookup(%s) classified a package nothing names", pkg)
		}
	}
}

func TestGitIsMatchedByNameOrPathBase(t *testing.T) {
	class, err := parseClassification([]byte(`{"git": ["git"], "packages": {}}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"git":              true,
		"/usr/bin/git":     true,
		"/opt/git/bin/git": true,
		"gitk":             false,
		"/usr/bin/git-lfs": false,
		"/git/bin/true":    false,
	} {
		if got := class.isGit(name); got != want {
			t.Errorf("isGit(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestTheEmbeddedClassificationParses(t *testing.T) {
	if _, err := parseClassification(classificationData); err != nil {
		t.Fatal(err)
	}
}
