package workflows

import (
	"regexp"
	"testing"
)

// Event names are the stable contract user definitions bind to: snake_case
// semantic names, unique, and every catalog entry self-reports as known.
func TestCatalogNamesAreStableContractShaped(t *testing.T) {
	namePattern := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	seen := map[Event]bool{}
	for _, event := range Catalog() {
		if !namePattern.MatchString(string(event)) {
			t.Errorf("event %q is not snake_case", event)
		}
		if seen[event] {
			t.Errorf("event %q appears twice in the catalog", event)
		}
		seen[event] = true
		if !event.Known() {
			t.Errorf("catalog event %q not Known()", event)
		}
	}
	if len(seen) == 0 {
		t.Fatal("Catalog() is empty")
	}
	if Event("not_a_real_event").Known() {
		t.Fatal("Known() accepted an event outside the catalog")
	}
}
