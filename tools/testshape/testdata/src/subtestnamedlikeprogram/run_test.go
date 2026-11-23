// Package subtestnamedlikeprogram runs a subtest named like a program.
package subtestnamedlikeprogram

import "testing"

// t.Run and a subtest that only computes: the name "git" spawns nothing.
func TestASubtestNamedGitIsUnit(t *testing.T) {
	t.Run("git", func(t *testing.T) {
		if 1+1 != 2 {
			t.Fatal("arithmetic")
		}
	})
}
