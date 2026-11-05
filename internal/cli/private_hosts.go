package cli

import (
	"context"

	"github.com/shaul/mesh/internal/protocol"
)

// existingService reads the route's current definition from the host itself.
// A failed read is an error: the local cache does not hold recipes, so it
// cannot say what an edit would inherit.
func (a *application) existingService(ctx context.Context, host HostRecord, name string) (protocol.ServiceInfo, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, serviceMutationTimeout)
	defer cancel()
	snapshot, err := listRemoteServices(ctx, host, a.dependencies.DialControl)
	if err != nil {
		return protocol.ServiceInfo{}, false, err
	}
	for _, service := range snapshot.Services {
		if service.Name == name {
			return service, true, nil
		}
	}
	return protocol.ServiceInfo{}, false, nil
}
