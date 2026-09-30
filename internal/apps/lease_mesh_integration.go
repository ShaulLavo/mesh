//go:build mesh_integration

package apps

import "time"

func init() {
	LeaseTTL = 3 * time.Second
}
