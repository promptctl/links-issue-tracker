package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// UpSQL is the SQL goose runs for data's Up: the lines after the Up annotation
// and before the Down annotation. The store parses a migration's DDL from it.
func UpSQL(data []byte) string {
	return split(data).up
}

// UpIdentity is the hex sha256 of everything goose reads before data's Down
// annotation, the header and the Up annotation included, plus whether goose
// runs the file outside a transaction. Two files with one UpIdentity migrate a
// store identically, whatever their Downs; a released migration is released
// exactly when its UpIdentity is, so its Down can be fixed in place.
//
// [LAW:one-source-of-truth] the one released identity of a migration: the
// release pin, the source-install guard, and the released-schema ceiling all
// compare it.
func UpIdentity(data []byte) string {
	s := split(data)
	head := s.head
	if s.noTransaction {
		head += "\n-- +goose NO TRANSACTION"
	}
	sum := sha256.Sum256([]byte(head))
	return hex.EncodeToString(sum[:])
}

// sections is data divided where goose's sqlparser divides it. Neither head
// nor up holds a NO TRANSACTION line; goose applies that annotation to the
// whole file wherever it sits, so it is carried as noTransaction.
type sections struct {
	head, up      string
	noTransaction bool
}

// split recognizes annotations by goose v3's own test: a line starting `--`
// that contains `+goose`, with no comment or quote tracking. It reads only
// files goose accepts the same way goose does; a file goose refuses (an
// indented annotation, a second Up) is refused when goose runs it, and the
// registry's migration tests run every file.
func split(data []byte) sections {
	var head, up []string
	var s sections
	inUp, inDown := false, false
	for _, line := range strings.Split(string(data), "\n") {
		annotation, ok := gooseAnnotation(line)
		switch {
		case ok && strings.EqualFold(annotation, "NO TRANSACTION"):
			s.noTransaction = true
			continue
		case ok && strings.EqualFold(annotation, "Down"):
			inDown = true
		case ok && strings.EqualFold(annotation, "Up") && !inDown:
			head = append(head, line)
			inUp = true
			continue
		}
		if inDown {
			continue
		}
		head = append(head, line)
		if inUp {
			up = append(up, line)
		}
	}
	s.head = strings.Join(head, "\n")
	s.up = strings.Join(up, "\n")
	return s
}

// gooseAnnotation returns the annotation a line carries as goose's sqlparser
// reads it: a line whose trimmed text starts with `--` and that contains
// `+goose`, with every `--` and the first `+goose` removed.
func gooseAnnotation(line string) (string, bool) {
	if !strings.HasPrefix(strings.TrimSpace(line), "--") || !strings.Contains(line, "+goose") {
		return "", false
	}
	cmd := strings.Replace(strings.ReplaceAll(line, "--", ""), "+goose", "", 1)
	return strings.TrimSpace(cmd), true
}
