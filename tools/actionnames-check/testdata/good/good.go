// Package good formats an ActionName only as the verb the caller typed.
package good

import (
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

func Refuse(a model.ActionName) error {
	return fmt.Errorf("cannot `%s`", a.Verb())
}
