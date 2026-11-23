// Package transitiveinit imports front, whose own dependency deep has an
// initializer that touches the filesystem.
package transitiveinit

import "example.com/shape/nested/front"

func Home() string { return front.Home() }
