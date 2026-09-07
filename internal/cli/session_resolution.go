package cli

import (
	"path/filepath"

	"github.com/shaul/mesh/internal/identity"
)

func withoutKnownSelfHost(hosts []HostRecord, sessionDir string) []HostRecord {
	local, err := identity.Load(filepath.Dir(filepath.Dir(sessionDir)))
	if err != nil {
		return hosts
	}
	remote := make([]HostRecord, 0, len(hosts))
	for _, host := range hosts {
		if host.ID == local.ID && host.MeshIdentity == local.ID {
			continue
		}
		remote = append(remote, host)
	}
	return remote
}
