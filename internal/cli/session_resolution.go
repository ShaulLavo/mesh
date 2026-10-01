package cli

func withoutThisHost(stateDir string, hosts []HostRecord) ([]HostRecord, string) {
	self, err := existingLocalIdentity(stateDir)
	if err != nil || self.ID == "" {
		return hosts, localHostAlias
	}
	alias := localHostAlias
	remote := make([]HostRecord, 0, len(hosts))
	for _, host := range hosts {
		if host.ID == self.ID && host.MeshIdentity == self.ID {
			alias = host.Alias
			continue
		}
		remote = append(remote, host)
	}
	return remote, alias
}
