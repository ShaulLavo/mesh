package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/shaul/mesh/internal/identity"
)

// existingLocalIdentity leaves a fresh or keyless host uninitialized. Only
// session creation and other explicit mutations may establish a new identity.
func existingLocalIdentity(stateDir string) (identity.Host, error) {
	host, err := identity.Load(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return identity.Host{}, nil
	}
	if err != nil {
		return identity.Host{}, fmt.Errorf("read local host identity: %w", err)
	}
	return host, nil
}
