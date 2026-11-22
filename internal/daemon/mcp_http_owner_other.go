//go:build !linux && !darwin

package daemon

import (
	"context"
	"fmt"
	"net"
	"net/netip"

	"github.com/shaul/mesh/internal/mcphost"
)

func (m mcpLauncher) CheckPortFree(context.Context, []netip.AddrPort) error {
	return fmt.Errorf("%w: managed HTTP listener verification is unsupported on this platform", mcphost.ErrManagedListenerMismatch)
}

func (m mcpLauncher) VerifyManagedDial(context.Context, mcphost.WorkerRef, netip.AddrPort, func() (net.Conn, error)) (net.Conn, error) {
	return nil, fmt.Errorf("%w: managed HTTP listener verification is unsupported on this platform", mcphost.ErrManagedListenerMismatch)
}
