package bad

import (
	"fmt"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Typed formats the verb, as every GOOS's load must see one site do.
func Typed(a model.ActionName) string { return fmt.Sprintf("%s", a.Verb()) }
