package cli

import "context"

func (a *application) existingPrivateHost(ctx context.Context, host HostRecord, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, serviceMutationTimeout)
	defer cancel()
	snapshot, err := listRemoteServices(ctx, host, a.dependencies.DialControl)
	if err != nil {
		return "", err
	}
	for _, service := range snapshot.Services {
		if service.Name == name {
			return service.PrivateHost, nil
		}
	}
	return "", nil
}
