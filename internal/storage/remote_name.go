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
		return RemoteName{}, RefusedRemoteNameError{Name: name, Character: name[at : at+1]}
	}
	return RemoteName{name: name}, nil
}

// RefusedRemoteNameError is ParseRemoteName's refusal of a name Dolt cannot
// hold: lit neither adds such a remote to Dolt nor syncs with it.
type RefusedRemoteNameError struct {
	Name, Character string
}

func (e RefusedRemoteNameError) Error() string {
	return fmt.Sprintf("remote %q has %q in its name, which Dolt does not allow in a remote's name; rename it (git remote rename) to sync with it", e.Name, e.Character)
}
