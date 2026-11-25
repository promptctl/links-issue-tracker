package workflows

import (
	"regexp"
	"testing"
)

// Event names are the stable contract user definitions bind to: snake_case
// semantic names, unique, and each reads back from a when: entry as itself.
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
		// A when: entry naming the event reads back as the event, never as a
		// crossing of some state.
		if moment, err := ParseMoment(string(event)); err != nil || moment != event {
			t.Errorf("ParseMoment(%q) = %#v, %v; want the event", event, moment, err)
		}
	}
	if len(seen) == 0 {
		t.Fatal("Catalog() is empty")
	}
}
