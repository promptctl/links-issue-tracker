package storage

import (
	"errors"
	"strings"

	"github.com/promptctl/links-issue-tracker/internal/model"
)

// Title is an issue's title as a write takes it: trimmed, never blank, and on
// one line. Only ParseTitle builds one that holds text, so a title is parsed
// once, where it enters. The zero value holds none: Go lets any code write it
// without parsing, so Text tells it from a parsed title and refuses it rather
// than let a blank title be stored. [LAW:parse-dont-validate]
// [LAW:no-silent-failure]
type Title struct {
	text string
}

// ParseTitle is the gate a title passes through on its way in: a create's
// title, and an update's title when it moves the issue to text it does not
// already hold.
func ParseTitle(raw string) (Title, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return Title{}, errTitleRequired
	}
	if strings.ContainsAny(text, lineBreaks) {
		return Title{}, model.ValidationError{Message: "title cannot hold a line break"}
	}
	return Title{text: text}, nil
}

// MustParseTitle is ParseTitle of a text the program itself states, which
// cannot be refused without the program being wrong.
func MustParseTitle(raw string) Title {
	title, err := ParseTitle(raw)
	if err != nil {
		panic(err)
	}
	return title
}

// Text is the title's text, and the one way to read it: an engine storing a
// title reads it here, so none can store the zero value's empty text.
func (t Title) Text() (string, error) {
	if t.text == "" {
		return "", errTitleRequired
	}
	return t.text, nil
}

// lineBreaks is every character a terminal or renderer may break a line at,
// not only \r and \n: a title or lane holding any of them prints as two rows.
const lineBreaks = "\r\n\v\f\u0085\u2028\u2029"

var errTitleRequired = model.ValidationError{Message: "title is required"}

// Lane is an issue's lane as a write takes it: trimmed and on one line. The
// empty lane is the shared lane, a value a write may store, so a parsed Lane
// records that a parser made it rather than reading the zero value's empty
// text as the shared lane. [LAW:parse-dont-validate]
type Lane struct {
	text string
	made bool
}

// ParseLane is the gate a lane passes through on its way in, as ParseTitle is
// a title's.
func ParseLane(raw string) (Lane, error) {
	text := strings.TrimSpace(raw)
	if strings.ContainsAny(text, lineBreaks) {
		return Lane{}, model.ValidationError{Message: "lane cannot hold a line break"}
	}
	return Lane{text: text, made: true}, nil
}

// MustParseLane is ParseLane of a text the program itself states.
func MustParseLane(raw string) Lane {
	lane, err := ParseLane(raw)
	if err != nil {
		panic(err)
	}
	return lane
}

// SharedLane is the shared lane, parsed.
var SharedLane = MustParseLane("")

// Text is the lane's text, and the one way to read it. A Lane no parser made
// is a create built without stating its lane — the program's mistake, not the
// author's — so it is refused as one rather than stored as the shared lane.
func (l Lane) Text() (string, error) {
	if !l.made {
		return "", errors.New("lane: the create states a lane no parser made")
	}
	return l.text, nil
}
