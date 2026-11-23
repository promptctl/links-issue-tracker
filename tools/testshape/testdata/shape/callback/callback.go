// Package callback hands an outside value to fmt, which calls its String.
package callback

import (
	"fmt"
	"net/url"
)

// Print renders u through fmt.
func Print(u *url.URL) string { return fmt.Sprint(u) }
