package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kevinburke/ssh_config"
)

// sshConfigHost is one Host alias a person could adopt, as their ssh config
// describes it.
type sshConfigHost struct {
	Alias    string
	HostName string
	User     string
}

// sshConfigHosts lists the concrete Host aliases in ~/.ssh/config. Mesh
// resolves these when adopting, so they are the targets most likely to work,
// and a tailnet listing alone hides every machine reached another way.
//
// Wildcards and negations are skipped: `Host *` sets defaults for everything
// and is not a machine anyone can adopt.
func sshConfigHosts() []sshConfigHost {
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

	seen := make(map[string]bool)
	var hosts []sshConfigHost
	for _, host := range config.Hosts {
		for _, pattern := range host.Patterns {
			alias := pattern.String()
			if alias == "" || seen[alias] || strings.ContainsAny(alias, "*?!") {
				continue
			}
			seen[alias] = true
			hostName, _ := config.Get(alias, "HostName")
			user, _ := config.Get(alias, "User")
			hosts = append(hosts, sshConfigHost{
				Alias:    alias,
				HostName: strings.TrimSpace(hostName),
				User:     strings.TrimSpace(user),
			})
		}
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Alias < hosts[j].Alias })
	return hosts
}
