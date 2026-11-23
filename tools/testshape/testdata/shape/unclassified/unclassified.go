// Package unclassified calls an outside function the fixture classification
// does not cover.
package unclassified

import "strings"

func Shout(s string) string { return strings.ToUpper(s) }
