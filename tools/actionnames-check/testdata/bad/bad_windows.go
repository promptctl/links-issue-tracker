// Package bad formats an ActionName as its persisted encoding, in a file
// only windows builds.
package bad

import (
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func Refuse(a model.ActionName) error {
	return fmt.Errorf("cannot `%s` (%s)", a.Verb(), a)
}
