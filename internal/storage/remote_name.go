package storage

import (
	"errors"
	"fmt"
	"strings"
)

// InvalidRemoteNameCharacters are the characters a remote's name may not
// hold: the ones Dolt refuses in DOLT_REMOTE add (doltenv's constant of the
// same name, which a store test holds this equal to). "/" is among them, so a
// remote's tracking refs remotes/<remote>/<branch> and its fork points
// lit-fork-point/<remote>/<branch> are read off the first segment and belong
// to that one remote. [LAW:one-source-of-truth]
const InvalidRemoteNameCharacters = " \t\n\r./\\!@#$%^&*(){}[],.<>'\"?=+|"

// RemoteName is the name of a remote lit syncs with. Only ParseRemoteName
// builds one that names a remote, and every Syncer method that syncs with a
// remote takes one, so a name is parsed where it enters and is never checked
// again. The zero value names none: it is what a caller holds before a
// remote is selected, never a remote to sync with. [LAW:parse-dont-validate]
type RemoteName struct {
	name string
}

func (r RemoteName) String() string {
	return r.name
}

// ParseRemoteName is the one parser of a remote's name. It refuses what Dolt
// refuses, naming the character, where Dolt says only "remote name invalid".
func ParseRemoteName(raw string) (RemoteName, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return RemoteName{}, errors.New("remote is required")
	}
	if at := strings.IndexAny(name, InvalidRemoteNameCharacters); at >= 0 {
		return RemoteName{}, RefusedRemoteError{Name: name, Reason: NameCharacter{Character: name[at : at+1]}}
	}
	return RemoteName{name: name}, nil
}

// RefusedRemoteError is a git remote lit neither adds to Dolt nor syncs with,
// and why. ParseRemoteName refuses names and workspace's reading of git's
// remote list refuses URLs, both as this one type, so every caller that tells
// a refused remote from a failure asks one question. [LAW:one-source-of-truth]
type RefusedRemoteError struct {
	Name   string
	Reason RemoteRefusal
}

func (e RefusedRemoteError) Error() string {
	return fmt.Sprintf("remote %q %s; %s to sync with it", e.Name, e.Reason.why(), e.Reason.remedy())
}

// RemoteRefusal is why a remote is refused. The set is closed: its methods are
// unexported, so only this package defines a reason. [LAW:types-are-the-program]
type RemoteRefusal interface {
	// Kind names the reason on the command's event.
	Kind() string
	why() string
	remedy() string
}

// NameCharacter: the name holds a character Dolt refuses in a remote's name.
type NameCharacter struct{ Character string }

func (r NameCharacter) Kind() string { return "name_character" }
func (r NameCharacter) why() string {
	return fmt.Sprintf("has %q in its name, which Dolt does not allow in a remote's name", r.Character)
}
func (NameCharacter) remedy() string { return "rename it (git remote rename)" }

// PathThroughFilesystem: a local path with a ".." after a directory name. The
// OS resolves a symlink before the "..", while every lexical step on the way
// to Dolt cleans it first, so the two can name different repositories.
type PathThroughFilesystem struct{ URL string }

func (r PathThroughFilesystem) Kind() string { return "path_through_filesystem" }
func (r PathThroughFilesystem) why() string {
	return fmt.Sprintf("has URL %q, whose \"..\" after a directory name only the filesystem can resolve", r.URL)
}
func (PathThroughFilesystem) remedy() string {
	return `write the URL without that ".." (git remote set-url)`
}

// DriveRelativePath: a Windows path with a drive and no root (C:x.git), which
// names a different repository from each directory it is read in.
type DriveRelativePath struct{ URL string }

func (r DriveRelativePath) Kind() string { return "drive_relative_path" }
func (r DriveRelativePath) why() string {
	return fmt.Sprintf("has URL %q, a drive-relative path that names a different repository from each directory", r.URL)
}
func (DriveRelativePath) remedy() string { return "write it as an absolute path (git remote set-url)" }

// HomeRelativePath: a local path git reads against a home directory (~/x.git).
type HomeRelativePath struct{ URL string }

func (r HomeRelativePath) Kind() string { return "home_relative_path" }
func (r HomeRelativePath) why() string {
	return fmt.Sprintf("has URL %q, which git reads against a home directory", r.URL)
}
func (HomeRelativePath) remedy() string { return "write it as an absolute path (git remote set-url)" }

// SurroundingWhitespace: an empty URL, or one with whitespace at an end. git
// keeps the whitespace and Dolt trims it, so the two name different
// repositories.
type SurroundingWhitespace struct{ URL string }

func (r SurroundingWhitespace) Kind() string { return "surrounding_whitespace" }
func (r SurroundingWhitespace) why() string {
	return fmt.Sprintf("has URL %q, which is empty or has whitespace at an end, which git keeps and Dolt trims", r.URL)
}
func (SurroundingWhitespace) remedy() string { return "write the URL without it (git remote set-url)" }
