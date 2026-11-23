package openedatinit

import "testing"

// Using db reaches openStore, the code that computed it.
func TestAMethodOfAVariableOpenedAtInitTouchesTheFilesystem(t *testing.T) { _ = db.Count() }
