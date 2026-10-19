package statusline

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The cc-candybar plugin lit ships, where `cc-candybar plugin install`
// reads it: .cc-candybar/plugins/lit/ at the repository's root.
var pluginDefinition = filepath.Join("..", "..", ".cc-candybar", "plugins", "lit", "plugin.json5")

// The definition checks the schema_version SchemaVersion writes, and names it
// in the warning it shows for any other, so changing the schema without the
// definition that reads it fails here.
func TestThePluginReadsTheSchemaVersionThisLitWrites(t *testing.T) {
	raw, err := os.ReadFile(pluginDefinition)
	if err != nil {
		t.Fatal(err)
	}
	version := strconv.Itoa(SchemaVersion)
	for _, anchor := range []string{
		`ne (toString .schema_version) "` + version + `"`,
		`this plugin reads ` + version + ` `,
	} {
		if n := strings.Count(string(raw), anchor); n != 1 {
			t.Errorf("plugin definition holds %q %d times, want exactly once", anchor, n)
		}
	}
}
