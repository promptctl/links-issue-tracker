package selector

import (
	"fmt"
	"strings"
)

// Example is a selector shown in the reference, with what it selects.
type Example struct {
	Text string
	Says string
}

// Describe is the whole reference a reader learns selectors from: the grammar,
// what each kind admits, every field of the schema's tables with its kind, and
// the examples. Everything about a field is read from its table, so a field
// added there is described here with no other edit. [LAW:one-source-of-truth]
func Describe(schema Schema, examples []Example) string {
	var b strings.Builder
	b.WriteString(`A selector is atoms separated by spaces, and every atom must hold:
  path            the path reads a value
  !path           the path reads no value
  path OP v1,v2   some value the path reads satisfies OP with some value listed
OP is one of = != ~ < <= > >=. != holds exactly where = does not, so it holds
when the path reads nothing. ~ is contains, ignoring case. < <= > >= take one
value. A path is fields joined by /: a field whose values are rows reads on
into their fields, takes (selector) to keep only the rows it selects, and a
list ends in /count to compare how many distinct values it reads. A path that
starts with / reads from the workspace instead of from the row.
Quote a value with "..." to hold spaces, commas, parentheses or quotes (\" and
\\ inside quotes). A stamp is an instant like 2026-01-02T15:04:05Z, or now,
now-6h, now+2d (units m h d w), read once when the command starts.

Kinds:
`)
	for _, kind := range []Kind{Text, Enum, Stamp, Search, Ref, Rows, Texts, Records} {
		writeKind(&b, kind.rule())
	}
	writeKind(&b, countRule)
	writeTable(&b, schema.Root, "Fields of "+articled(schema.Root.name)+":")
	if schema.Anchor != nil {
		writeTable(&b, schema.Anchor, "Fields of the "+schema.Anchor.name+", after a leading /:")
	}
	if len(examples) > 0 {
		b.WriteString("\nExamples:\n")
		width := 0
		for _, e := range examples {
			width = max(width, len(e.Text))
		}
		for _, e := range examples {
			fmt.Fprintf(&b, "  %-*s  %s\n", width, e.Text, e.Says)
		}
	}
	return b.String()
}

func writeKind(b *strings.Builder, r rule) {
	forms := formList(r.ops)
	if r.list {
		forms += " /count"
	}
	fmt.Fprintf(b, "  %-8s %-28s %s\n", r.name, forms, r.says)
}

func writeTable(b *strings.Builder, t *Table, heading string) {
	fmt.Fprintf(b, "\n%s\n", heading)
	for _, f := range t.fields {
		doc := f.doc
		if len(f.members) > 0 {
			doc += " One of " + strings.Join(f.members, ", ") + "."
		}
		fmt.Fprintf(b, "  %-16s %-8s %s\n", f.name, f.kind.rule().name, doc)
	}
}

func articled(name string) string {
	if strings.ContainsRune("aeiou", rune(name[0])) {
		return "an " + name
	}
	return "a " + name
}
