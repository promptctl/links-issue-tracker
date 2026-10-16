package statusline

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The definition installs where cc-candybar's docs say an app's plugin goes:
// a folder named for the plugin under the data home's cc-candybar/plugins.
func TestPluginPathIsCandybarsInstalledPluginRoot(t *testing.T) {
	got := PluginPath("/home/u/.local/share")
	want := "/home/u/.local/share/cc-candybar/plugins/lit/plugin.json5"
	if got != filepath.FromSlash(want) {
		t.Fatalf("PluginPath = %s, want %s", got, want)
	}
}

// The first install writes the definition, a second finds it there and writes
// nothing, and a definition another lit left is replaced with this one's.
func TestInstallPluginWritesTheDefinitionOnlyWhenItDiffers(t *testing.T) {
	path := PluginPath(t.TempDir())
	installed := func(want bool) {
		t.Helper()
		got, err := InstallPlugin(path)
		if err != nil {
			t.Fatalf("InstallPlugin: %v", err)
		}
		if got != want {
			t.Fatalf("InstallPlugin wrote = %t, want %t", got, want)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, PluginDefinition) {
			t.Fatalf("installed plugin differs from the embedded definition:\n%s", raw)
		}
	}
	installed(true)
	installed(false)
	if err := os.WriteFile(path, []byte("{ segments: {} }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installed(true)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("plugin folder holds %d entries, want only plugin.json5: %v", len(entries), entries)
	}
}

// A definition that cannot be read is an error, never a reason to write over it
// blind.
func TestInstallPluginReportsAnUnreadableDefinition(t *testing.T) {
	path := PluginPath(t.TempDir())
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallPlugin(path); err == nil || !strings.Contains(err.Error(), "read cc-candybar plugin") {
		t.Fatalf("InstallPlugin over a directory: err = %v, want a read error", err)
	}
}

// The definition checks the schema_version SchemaVersion writes, and names it
// in the warning it shows for any other, so changing the schema without the
// definition that reads it fails here.
func TestThePluginReadsTheSchemaVersionThisLitWrites(t *testing.T) {
	version := strconv.Itoa(SchemaVersion)
	for _, anchor := range []string{
		`ne (toString .schema_version) "` + version + `"`,
		`this plugin reads ` + version + ` `,
	} {
		if n := strings.Count(string(PluginDefinition), anchor); n != 1 {
			t.Errorf("plugin definition holds %q %d times, want exactly once", anchor, n)
		}
	}
}
