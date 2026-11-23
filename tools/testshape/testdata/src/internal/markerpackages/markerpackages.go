// Package markerpackages imports every package a marker names a function
// of, so the fixture program declares them all, as the measured module's
// program does.
package markerpackages

import (
	_ "context"
	_ "database/sql"
	_ "go/importer"
	_ "io/ioutil"
	_ "net"
	_ "net/http"
	_ "net/http/httptest"
	_ "os"
	_ "os/exec"
	_ "path/filepath"
	_ "syscall"
	_ "testing"
	_ "time"

	_ "github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	_ "github.com/dolthub/dolt/go/libraries/utils/gitcmd"
	_ "golang.org/x/tools/go/packages"
)
