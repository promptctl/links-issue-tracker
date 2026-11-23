// Package trun writes a file, which its test does inside a subtest.
package trun

import "os"

func Write(name string) error { return os.WriteFile(name, nil, 0o644) }
