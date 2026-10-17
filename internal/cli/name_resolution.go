package cli

import (
	"context"
	"fmt"
)

func resolveArgumentWithRefresh(ctx context.Context, value string, hosts []HostRecord, dial HostDialer) (ArgumentTarget, error) {
	target, err := ResolveArgument(value, hosts)
	if err == nil {
		return target, nil
	}
	syncContext, cancel := context.WithTimeout(ctx, remoteConnectTimeout)
	_ = syncFleetHosts(syncContext)
	cancel()
	hosts, err = LoadHosts()
	if err != nil {
		return ArgumentTarget{}, err
	}
	refreshKnownHostNames(ctx, hosts, dial)
	if err := ctx.Err(); err != nil {
		return ArgumentTarget{}, fmt.Errorf("refresh machine names: %w", err)
	}
	hosts, err = LoadHosts()
	if err != nil {
		return ArgumentTarget{}, err
	}
	return ResolveArgument(value, hosts)
}

func refreshKnownHostNames(parent context.Context, hosts []HostRecord, dial HostDialer) {
	ctx, cancel := context.WithTimeout(parent, remoteConnectTimeout)
	defer cancel()
	done := make(chan struct{}, len(hosts))
	for _, host := range hosts {
		go func() {
			refreshKnownHostName(ctx, host, dial)
			done <- struct{}{}
		}()
	}
	for range hosts {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
}

func refreshKnownHostName(ctx context.Context, host HostRecord, dial HostDialer) {
	conn, _, err := openVerifiedHostInfo(ctx, host, dial)
	if err == nil {
		_ = conn.Close()
	}
}
