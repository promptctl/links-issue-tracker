package migrations

import "strings"

// Sections divides a migration file where goose's sqlparser divides it. up is
// every line after the Up annotation and before the Down annotation; head is
// every line before the Down annotation, the header and the Up annotation
// included. Neither holds a `NO TRANSACTION` line; noTransaction reports a `NO TRANSACTION` annotation anywhere in
// the file, which goose applies to the Up and the Down alike.
//
// [LAW:one-source-of-truth] the one reading of a migration's Up: the store
// parses the Up's DDL from up, and the release pin hashes head.
type Sections struct {
	Head          string
	Up            string
	NoTransaction bool
}

// Split reads data's annotations exactly as goose v3's sqlparser does: no
// comment or quote tracking, and an indented annotation is not one.
func Split(data []byte) Sections {
	var head, up []string
	var s Sections
	inUp, inDown := false, false
	for _, line := range strings.Split(string(data), "\n") {
		annotation, ok := gooseAnnotation(line)
		switch {
		case ok && strings.EqualFold(annotation, "NO TRANSACTION"):
			s.NoTransaction = true
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
	s.Head = strings.Join(head, "\n")
	s.Up = strings.Join(up, "\n")
	return s
}

// gooseAnnotation returns the annotation a line carries as goose's
// sqlparser reads it: an unindented line whose trimmed text starts with `--`
// and that contains `+goose`, with every `--` and the first `+goose` removed.
func gooseAnnotation(line string) (string, bool) {
	if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") ||
		!strings.HasPrefix(strings.TrimSpace(line), "--") || !strings.Contains(line, "+goose") {
		return "", false
	}
	cmd := strings.Replace(strings.ReplaceAll(line, "--", ""), "+goose", "", 1)
	return strings.TrimSpace(cmd), true
}
