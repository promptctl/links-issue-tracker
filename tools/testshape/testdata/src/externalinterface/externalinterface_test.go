package externalinterface

import "testing"

// The *http.Client converted to doer carries its Do, a network marker.
func TestAnHTTPClientBehindAnInterfaceTouchesTheNetwork(t *testing.T) { Fetch(nil) }
