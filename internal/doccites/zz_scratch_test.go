package doccites

import (
	"testing"
	"testing/fstest"
)

func TestScratchLocalVarShadowsRealDecl(t *testing.T) {
	src := "package a\n\nfunc helper() {\n\tvar Widget int\n\t_ = Widget\n}\n\nfunc Widget() {\n\treturn\n}\n"
	fsys := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Widget` returns nothing (`internal/a/a.go:9`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte(src)},
	}
	f, err := Survey(fsys)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", f)
	t.Logf("String: %s", f[0])
}

// Comma-tail: one span holds, the sibling span is wrong -> does the gate notice?
func TestScratchCommaTailMasking(t *testing.T) {
	before := fstest.MapFS{
		"doc-v1-total/x.md": &fstest.MapFile{Data: []byte("`Widget` is the unit (`internal/a/a.go:3,5`).\n")},
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n\ntype Widget struct{}\n\nvar _ = Widget{}\n")},
	}
	fb, _ := Survey(before)
	t.Logf("before: %+v", fb)
	m := Holding(fb)
	t.Logf("manifest entries: %d %+v", len(m), m)

	// Now break the FIRST span only: move Widget's declaration away from line 3.
	after := fstest.MapFS{
		"doc-v1-total/x.md": before["doc-v1-total/x.md"],
		"internal/a/a.go":   &fstest.MapFile{Data: []byte("package a\n\n// pad\n\nvar _ = Widget{}\n\ntype Widget struct{}\n")},
	}
	fa, _ := Survey(after)
	t.Logf("after: %+v", fa)
	t.Logf("lapsed: %+v", Lapsed(m, fa))
}
