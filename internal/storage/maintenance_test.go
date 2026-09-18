package storage

import (
	"reflect"
	"testing"
)

// Unchecked names checks by the json key of the field they would have filled.
// That is a parallel list, and a parallel list drifts: rename a field and the
// constant still reads "rank_inversions" with nothing to catch it, so a report
// would name a check no reader can map back to a field. This pins each constant
// to the tag it claims. [LAW:one-source-of-truth]
func TestUncheckedNamesMatchJSONTags(t *testing.T) {
	tags := map[string]bool{}
	rt := reflect.TypeOf(HealthReport{})
	for i := 0; i < rt.NumField(); i++ {
		if tag, ok := rt.Field(i).Tag.Lookup("json"); ok {
			tags[tag] = true
		}
	}
	// Anti-vacuity: if the reflection found nothing, every assertion below would
	// be checking an empty set.
	if len(tags) == 0 {
		t.Fatal("no json tags found on HealthReport; the check below would be vacuous")
	}
	for _, name := range []string{CheckRankInversions, CheckDependencyCycle} {
		if !tags[name] {
			t.Errorf("Unchecked name %q is not a json tag on HealthReport; a reader cannot map it to a field", name)
		}
	}
}
