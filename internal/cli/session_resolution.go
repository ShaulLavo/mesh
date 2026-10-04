package cli

import "context"

func withoutThisHost(ctx context.Context, stateDir string, hosts []HostRecord) ([]HostRecord, string) {
	self, err := localNameRecord(ctx, stateDir)
	if err != nil || self.ID == "" {
		return hosts, "Local machine"
	}
	label := HostLabel(self)
	remote := make([]HostRecord, 0, len(hosts))
	for _, host := range hosts {
		if host.ID == self.ID && host.MeshIdentity == self.ID {
			continue
		}
		remote = append(remote, host)
	}
	return remote, label
}
