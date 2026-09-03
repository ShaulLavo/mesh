package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

// sshConfigIdentityFile is the key the ssh config names for an alias, expanded
// from ~ because ssh accepts it there and os.Open does not.
func sshConfigIdentityFile(alias string, resolve sshConfigResolver) string {
	if resolve == nil || alias == "" {
		return ""
	}
	identity := strings.TrimSpace(resolve(alias, "IdentityFile"))
	if identity == "" {
		return ""
	}
	if identity == "~" || strings.HasPrefix(identity, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		identity = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(identity, "~"), "/"))
	}
	return identity
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

// configuredUserHint explains an authentication failure caused by naming a user
// the ssh config would have chosen differently. Writing `mesh add me@pi` out of
// habit overrides a `User pi` the config already had right, and the resulting
// failure otherwise says only that no key worked.
func configuredUserHint(t target) string {
	if !t.explicitUser || t.alias == "" {
		return ""
	}
	resolve := userSSHConfig()
	if resolve == nil {
		return ""
	}
	configured := strings.TrimSpace(resolve(t.alias, "User"))
	if configured == "" || configured == t.user {
		return ""
	}
	return fmt.Sprintf("~/.ssh/config sets User %s for %s, and %s overrode it; try mesh add %s",
		configured, t.alias, t.user, t.alias)
}
