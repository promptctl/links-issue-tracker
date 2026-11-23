// Package doltload loads a Dolt database directly, around the module's
// openers.
package doltload

import (
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
)

func TestLoadingADoltDatabaseTouchesTheStore(t *testing.T) { doltdb.LoadDoltDBWithParams() }
