package identity

import (
	"encoding/hex"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// GrantIncarnation names a device's current approval line. Re-approving a
// device creates a new incarnation, so state bound to the old one (such as a
// hosted MCP continuation) cannot be reused after a revocation.
func GrantIncarnation(stateDir, id string) (string, bool) {
	public, err := IdentityKey(id)
	if err != nil {
		return "", false
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		return "", false
	}
	grant, ok := currentGrant(filepath.Join(stateDir, "authorized_keys"), key, nil)
	if !ok {
		return "", false
	}
	return hex.EncodeToString(grant.incarnation[:16]), true
}
