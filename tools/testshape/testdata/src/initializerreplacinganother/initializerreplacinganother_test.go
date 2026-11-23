package initializerreplacinganother

import "testing"

// disarm's store into open is no initializer's, so open still reaches what
// its initializer stored.
func TestAVariableReplacedByAnotherStillReachesItsInitializer(t *testing.T) { Run() }
