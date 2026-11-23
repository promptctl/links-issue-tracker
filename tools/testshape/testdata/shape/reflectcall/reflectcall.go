// Package reflectcall hands a closure to text/template in an interface
// value; a template calls the functions it is given by reflection.
package reflectcall

import (
	"os"
	"text/template"
)

func Save() {
	save := func(name string) string {
		_ = os.WriteFile(name, nil, 0o600)
		return name
	}
	_ = template.New("t").Funcs(template.FuncMap{"save": save})
}
