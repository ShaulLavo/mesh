// Package machinename owns command-safe destination names and their revisions.
package machinename

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/shaul/mesh/internal/session"
)

var reservedNames = map[string]struct{}{
	"add": {}, "app": {}, "attach": {}, "back": {}, "completion": {}, "daemon": {}, "gc": {}, "help": {}, "hibernate": {},
	"kill": {}, "rm": {}, "remove": {}, "rename": {}, "mv": {}, "list": {}, "local": {}, "logs": {}, "ls": {}, "man": {},
	"recover": {}, "recovery-command": {}, "shell-init": {}, "shell-update": {}, "agent": {}, "agent-hook": {}, "agent-resume": {}, "private-names": {}, "serve": {}, "session-worker": {}, "sig": {}, "signal": {}, "unserve": {}, "wake": {},
	"device": {}, "update": {}, "version": {}, "update-helper": {}, "update-notice-check": {}, "update-bootstrap": {},
	"update-bootstrap-status": {}, "dashboard": {},
}

// Claim is one destination's durable declaration, indexed by its stable identity.
type Claim struct {
	ID          string `json:"id"`
	MachineName string `json:"machineName"`
	Revision    uint64 `json:"revision"`
}

func Normalize(value string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(value))
	if name == "" {
		return "", errors.New("machine name is empty")
	}
	if len(name) > 63 {
		return "", errors.New("machine name is longer than 63 characters")
	}
	for i, character := range []byte(name) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' && i > 0 && i < len(name)-1 {
			continue
		}
		return "", errors.New("machine name must contain lowercase letters, digits, or interior hyphens")
	}
	if _, exists := reservedNames[name]; exists {
		return "", errors.New("machine name is a Mesh command; choose another name")
	}
	if _, err := session.ParseID(name); err == nil {
		return "", errors.New("machine name looks like a session ID; choose another name")
	}
	return name, nil
}

// Initial chooses a name once at the destination. Renames never reread OS naming.
func Initial(id string, candidates ...string) string {
	for _, candidate := range candidates {
		short, _, _ := strings.Cut(candidate, ".")
		if name, err := Normalize(short); err == nil {
			return name
		}
	}
	digest := sha256.Sum256([]byte(id))
	return fmt.Sprintf("host-%x", digest[:6])
}
