package statusline

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// PluginDefinition is lit's cc-candybar plugin: the segments that show this
// file on cc-candybar's bar. It ships in the binary that writes the file, so a
// SchemaVersion change and the definition that reads it cannot ship apart.
// Only cc-candybar can say whether its templates render; tools/candybar-check
// renders it on a pinned cc-candybar (`go run ./tools/candybar-check`, which
// needs npm).
//
//go:embed plugin/lit/plugin.json5
var PluginDefinition []byte

// PluginPath is where cc-candybar finds lit's plugin installed for the user
// whose XDG data home is dataHome. The folder's name, lit, is the plugin's.
func PluginPath(dataHome string) string {
	return filepath.Join(dataHome, "cc-candybar", "plugins", "lit", "plugin.json5")
}

// InstallPlugin puts PluginDefinition at path unless the file there already
// holds it, and reports whether it wrote. Whichever lit wrote the file last
// installs the definition it reads with, so a definition left by an older or
// newer lit is replaced, never kept. The write renames a complete file into
// place: cc-candybar watches the folder and never loads a partial one.
func InstallPlugin(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(raw, PluginDefinition):
		return false, nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("read cc-candybar plugin: %w", err)
	}
	if err := replaceFile(path, PluginDefinition); err != nil {
		return false, fmt.Errorf("install cc-candybar plugin: %w", err)
	}
	return true, nil
}
