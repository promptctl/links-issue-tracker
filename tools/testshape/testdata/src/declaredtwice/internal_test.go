// Package declaredtwice declares one test name in its package and in its
// external test package, so a stream's test of that name is either one.
package declaredtwice

import "testing"

func TestDeclaredTwice(t *testing.T) {}
