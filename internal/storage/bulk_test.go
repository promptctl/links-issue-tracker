package storage_test

import (
	"reflect"
	"testing"

	"github.com/promptctl/links-issue-tracker/internal/storage"
)

// A refused batch's account is what stays written: a create the rollback
// leaked stays, with the order the file gave it, and a create the rollback
// deleted goes, with its order. An update is never rolled back, so it stays.
func TestAfterRollbackKeepsTheCreatesTheRollbackLeaked(t *testing.T) {
	result := storage.BulkApplyResult{
		Created: []storage.IDMapping{storage.NewIDMapping("a", "t-a"), storage.NewIDMapping("b", "t-b")},
		Updated: []string{"t-u"},
		Ranked:  storage.RankSetResult{Resolutions: []storage.RankSetResolution{{RankedID: "t-a"}, {RankedID: "t-b"}, {RankedID: "t-u"}}},
	}
	kept := result.AfterRollback([]string{"t-b"})
	if want := []storage.IDMapping{storage.NewIDMapping("b", "t-b")}; !reflect.DeepEqual(kept.Created, want) {
		t.Errorf("AfterRollback(leaked t-b).Created = %v, want %v", kept.Created, want)
	}
	var ranked []string
	for _, res := range kept.Ranked.Resolutions {
		ranked = append(ranked, res.RankedID)
	}
	if want := []string{"t-b", "t-u"}; !reflect.DeepEqual(ranked, want) {
		t.Errorf("AfterRollback(leaked t-b) kept the order of %v, want %v", ranked, want)
	}
	if want := []string{"t-u"}; !reflect.DeepEqual(kept.Updated, want) {
		t.Errorf("AfterRollback(leaked t-b).Updated = %v, want %v", kept.Updated, want)
	}
}
