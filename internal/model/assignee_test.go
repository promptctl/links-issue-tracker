package model

import "testing"

// TestNormalizeAssigneeRefusesOnlyWhatNoRowCanCarry pins the assignee gate to
// the rule a title and a lane are written under: every line-break form and a
// comma are refused, and a name whose ZWNJ, emoji sequence or tab a printer
// holds stays writable, as it was stored.
func TestNormalizeAssigneeRefusesOnlyWhatNoRowCanCarry(t *testing.T) {
	t.Parallel()
	for _, refused := range []string{"a\nb", "a\r\nb", "a b", "a\u0085b", "a\vb", "Doe, J"} {
		if _, err := NormalizeAssignee(refused); err == nil {
			t.Errorf("NormalizeAssignee(%q) stored a value no row can carry", refused)
		}
	}
	for _, kept := range []string{"ada", "مهر‌نوش", "Ana👩‍💻", "a\tb", ""} {
		got, err := NormalizeAssignee(" " + kept + " ")
		if err != nil || got != kept {
			t.Errorf("NormalizeAssignee(%q) = %q, %v; want %q stored as written", kept, got, err, kept)
		}
	}
}
