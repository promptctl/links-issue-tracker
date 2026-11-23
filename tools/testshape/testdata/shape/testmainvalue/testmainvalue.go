// Package testmainvalue has a loader its TestMain hands to its tests through
// a package variable.
package testmainvalue

import "os"

func ReadConfig() ([]byte, error) { return os.ReadFile("config") }
