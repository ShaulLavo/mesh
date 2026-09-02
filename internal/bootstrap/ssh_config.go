package bootstrap

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/kevinburke/ssh_config"
)

// sshConfigResolver answers one directive for one Host alias, the way the ssh
// client would. A nil resolver means there is nothing to apply.
type sshConfigResolver func(alias, key string) string

// userSSHConfig reads ~/.ssh/config. A missing, unreadable, or malformed file
// resolves nothing rather than failing: adoption by address has to keep working
// on a machine with no ssh config at all.
func userSSHConfig() sshConfigResolver {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	contents, err := os.ReadFile(filepath.Join(home, ".ssh", "config")) //nolint:gosec // the fixed ssh config path under the caller's own home directory
	if err != nil {
		return nil
	}
	config, err := ssh_config.DecodeBytes(contents)
	if err != nil {
		return nil
	}
	return func(alias, key string) string {
		value, err := config.Get(alias, key)
		if err != nil {
			return ""
		}
		return value
	}
}

// applySSHConfig fills a target from the user's ssh config. Mesh dials the
// address itself rather than shelling out to ssh, so without this a Host alias
// that `ssh` resolves fails here as a DNS lookup for the alias itself, on a
// machine the user can plainly reach.
//
// An explicit user or port in the target always wins, matching ssh, where the
// command line overrides the config file.
func applySSHConfig(t target, resolve sshConfigResolver) target {
	if resolve == nil || t.alias == "" {
		return t
	}
	if hostname := resolve(t.alias, "HostName"); hostname != "" {
		t.host = hostname
	}
	if !t.explicitUser {
		if user := resolve(t.alias, "User"); user != "" {
			t.user = user
		}
	}
	if t.explicitPort {
		return t
	}
	text := resolve(t.alias, "Port")
	if text == "" {
		return t
	}
	port, err := strconv.ParseUint(text, 10, 16)
	if err != nil || port == 0 {
		return t
	}
	t.port = uint16(port)
	return t
}
