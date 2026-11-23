// Package bad formats an ActionName as its persisted encoding.
package bad

import (
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Typed formats the verb, as a load must see one site do.
func Typed(a model.ActionName) string { return fmt.Sprintf("%s", a.Verb()) }

func Refuse(a model.ActionName) error {
	return fmt.Errorf("cannot `%s` (%s)", a.Verb(), a)
}
