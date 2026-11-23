// Package nesteddep calls into a module nested under its own module's path.
package nesteddep

import "example.com/shape/nested"

func Connect() error {
	_, err := nested.Dial()
	return err
}
