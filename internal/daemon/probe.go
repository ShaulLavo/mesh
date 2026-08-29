package daemon

import (
	"context"
	"fmt"
	"net"
	"time"
)

type unixWorkerProbe struct {
	dialer net.Dialer
}

func newUnixWorkerProbe() unixWorkerProbe {
	return unixWorkerProbe{dialer: net.Dialer{Timeout: 500 * time.Millisecond}}
}

func (p unixWorkerProbe) Probe(ctx context.Context, socketPath string) error {
	if ctx == nil {
		return fmt.Errorf("daemon: probe %s with nil context", socketPath)
	}
	if socketPath == "" {
		return fmt.Errorf("daemon: probe empty worker socket path")
	}
	conn, err := p.dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("daemon: probe worker at %s: %w", socketPath, err)
	}
	if err := conn.Close(); err != nil {
		return fmt.Errorf("daemon: close worker probe at %s: %w", socketPath, err)
	}
	return nil
}

var _ WorkerProbe = unixWorkerProbe{}
