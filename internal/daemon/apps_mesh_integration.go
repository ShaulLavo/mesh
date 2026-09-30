//go:build mesh_integration

package daemon

import "time"

func init() {
	appSyncInterval = 500 * time.Millisecond
}
