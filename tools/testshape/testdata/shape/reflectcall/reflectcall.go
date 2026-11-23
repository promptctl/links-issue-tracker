// Package reflectcall calls a closure through reflect.Value.Call.
package reflectcall

import (
	"os"
	"reflect"
)

func Save() {
	save := func(name string) { _ = os.WriteFile(name, nil, 0o600) }
	reflect.ValueOf(save).Call([]reflect.Value{reflect.ValueOf("saved")})
}
