module example.com/fixtures

go 1.25

require (
	github.com/dolthub/dolt/go v0.0.0
	golang.org/x/tools v0.0.0
)

// The stubs declare the third-party functions markers.go names, so every
// marker names a function this program declares.
replace (
	github.com/dolthub/dolt/go => ./stubs/dolt
	golang.org/x/tools => ./stubs/tools
)
