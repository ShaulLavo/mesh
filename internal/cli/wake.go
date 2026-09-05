package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	meshdaemon "github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/wake"
	"github.com/shaul/mesh/internal/wakeclient"
)

const wakeConnectTimeout = wakeclient.Timeout

const wakeIntentTimeout = wakeConnectTimeout + 4*remoteConnectTimeout

func newWakeClient() (*wakeclient.Client, error) {
	stateDir, err := paths.StateDir()
	if err != nil {
		return nil, err
	}
	return wakeclient.New(stateDir, wakeclient.Options{Endpoints: configuredWakeEndpoints})
}

func configuredWakeEndpoints(context.Context) ([]string, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return nil, err
	}
	endpoints := make([]string, len(hosts))
	for i, host := range hosts {
		endpoints[i] = host.Endpoint
	}
	return endpoints, nil
}

func wakeTarget(host HostRecord) wakeclient.Target {
	return wakeclient.Target{ID: host.ID, Name: host.Alias, Endpoint: host.Endpoint}
}

func (a *application) wakeHost(ctx context.Context, host HostRecord, output io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, wakeConnectTimeout)
	defer cancel()
	if a.dependencies.Wake != nil {
		return a.dependencies.Wake(ctx, host)
	}
	client, err := newWakeClient()
	if err != nil {
		return err
	}
	result, err := client.Wake(ctx, wakeTarget(host))
	if err != nil {
		return fmt.Errorf("wake host %s: %w", host.Alias, err)
	}
	if result.AlreadyOnline {
		_, err = fmt.Fprintf(output, "%s is already online\n", host.Alias)
		return err
	}
	_, err = fmt.Fprintf(output, "wake packet for %s sent by %s\n", host.Alias, result.Sender)
	return err
}

func (a *application) intentDialer(output io.Writer) HostDialer {
	return func(ctx context.Context, host HostRecord) (transport.Conn, error) {
		probeCtx, cancel := context.WithTimeout(ctx, remoteConnectTimeout)
		conn, err := a.dependencies.DialHost(probeCtx, host)
		cancel()
		if err == nil || ctx.Err() != nil {
			return conn, err
		}
		if wakeErr := a.wakeHost(ctx, host, output); wakeErr != nil {
			return nil, errors.Join(err, wakeErr)
		}
		connectCtx, connectCancel := context.WithTimeout(ctx, remoteConnectTimeout)
		defer connectCancel()
		return a.dependencies.DialHost(connectCtx, host)
	}
}

func recoverHost(ctx context.Context, host HostRecord) error {
	client, err := newWakeClient()
	if err != nil {
		return err
	}
	return client.Recover(ctx, wakeTarget(host))
}

func rememberWakeGrant(ctx context.Context, grant *wake.Grant) {
	if grant == nil {
		return
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return
	}
	cache, err := wake.NewCache(stateDir)
	if err != nil {
		return
	}
	// Wake caching must not make an otherwise usable terminal unavailable.
	cacheCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_ = cache.PutContext(cacheCtx, *grant)
}

func (a *application) configureWakeCommand(allowed bool) *cobra.Command {
	verb := "deny"
	short := "Stop allowing Mesh machines to wake this host"
	if allowed {
		verb = "allow"
		short = "Allow Mesh machines to wake this host"
	}
	return &cobra.Command{
		Use: verb, Short: short, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return configureWake(cmd, allowed)
		},
	}
}

func configureWake(cmd *cobra.Command, allowed bool) error {
	stateDir, err := paths.StateDir()
	if err != nil {
		return err
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
	defer cancel()
	response, err := daemonControlRequest(ctx, meshdaemon.SocketPath(stateDir), protocol.Control{
		Type: protocol.TypeWakeConfigure, RequestID: requestID, WakeAllowed: &allowed,
	})
	if err != nil {
		return err
	}
	if response.Type == protocol.TypeError {
		return daemonResponseError("configure wake permission", response.Message)
	}
	if response.Type != protocol.TypeWakeConfigured {
		return errors.New("daemon returned an unexpected wake configuration response")
	}
	rememberWakeGrant(ctx, response.WakeGrant)
	if !allowed {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Wake permission disabled on this host")
		return err
	}
	if response.WakeGrant == nil || !response.WakeGrant.Enabled || response.WakeGrant.NIC == nil {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Wake permission saved; waking is unavailable until a wired network is discovered")
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Mesh machines may now wake this host")
	return err
}
