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
// builds one. [LAW:parse-dont-validate]
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
		return RemoteName{}, fmt.Errorf("remote %q has %q in its name, which Dolt does not allow in a remote's name; rename it (git remote rename) to sync with it", name, name[at:at+1])
	}
	return RemoteName{name: name}, nil
}
