package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/tunnel"
)

func (a *application) serveClaimCommand() *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:   "claim EDGE FULLNAME",
		Short: "Reserve an exact public hostname for an SSH reverse tunnel",
		Args:  exactArgs(2, "an edge and full public hostname", "mesh serve claim vps blog.shaulavo.dev"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runTunnelClaim(cmd, args[0], args[1], yes)
		},
	}
	command.Flags().BoolVar(&yes, "yes", false, "skip the public confirmation prompt")
	return command
}

func tunnelEdge(hostAlias, publicName string) (HostRecord, error) {
	if err := tunnel.ValidateHostname(publicName); err != nil {
		return HostRecord{}, err
	}
	if hostAlias == "" {
		return HostRecord{}, errors.New("releasing a tunnel claim requires --host EDGE or --local-edge")
	}
	hosts, err := LoadHosts()
	if err != nil {
		return HostRecord{}, err
	}
	host, err := hostWithAlias(hosts, hostAlias)
	if err != nil {
		return HostRecord{}, err
	}
	if _, err := tunnel.PublicKey(host.MeshIdentity); err != nil {
		return HostRecord{}, fmt.Errorf("edge %s has an invalid pinned Mesh identity: %w", host.Alias, err)
	}
	return host, nil
}

func (a *application) runTunnelClaim(cmd *cobra.Command, hostAlias, publicName string, yes bool) error {
	host, err := tunnelEdge(hostAlias, publicName)
	if err != nil {
		return err
	}
	if !yes {
		confirmed, err := a.dependencies.ConfirmPublic(cmd.Context(), PublicConfirmation{
			Host: host, TunnelClaim: true, URL: "https://" + publicName,
		})
		if err != nil {
			return err
		}
		if !confirmed {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "reservation cancelled")
			return err
		}
	}
	stateDir, err := a.deliverTunnelMutation(cmd.Context(), host, tunnel.Create, publicName)
	if err != nil {
		return err
	}
	endpoint, err := url.Parse(host.Endpoint)
	if err != nil {
		return err
	}
	destination := host.TailscaleName
	if destination == "" {
		destination = endpoint.Hostname()
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "claimed %s on %s\nssh -N -o ExitOnForwardFailure=yes -o IdentitiesOnly=yes -i %s -p 2222 -R %s %s\n",
		publicName, host.Alias, tunnelShellQuote(filepath.Join(stateDir, "identity.key")),
		tunnelShellQuote(publicName+":80:localhost:3000"), tunnelShellQuote(destination))
	return err
}

func tunnelShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (a *application) runTunnelRelease(cmd *cobra.Command, publicName, hostAlias string) error {
	host, err := tunnelEdge(hostAlias, publicName)
	if err != nil {
		return err
	}
	if _, err := a.deliverTunnelMutation(cmd.Context(), host, tunnel.Release, publicName); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "released %s on %s\n", publicName, host.Alias)
	return err
}

func (a *application) deliverTunnelMutation(ctx context.Context, host HostRecord, action tunnel.Action, publicName string) (string, error) {
	stateDir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	_, key, err := identity.LoadPrivate(stateDir)
	if err != nil {
		return "", fmt.Errorf("load local Mesh identity: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, serviceMutationTimeout)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(stateDir, catalogDatabaseName))
	if err != nil {
		return "", err
	}
	defer store.Close() //nolint:errcheck // the mutation result is authoritative
	ack, err := store.DeliverTunnelMutation(ctx, host.MeshIdentity, key, action, publicName,
		func(ctx context.Context, mutation tunnel.Mutation) (tunnel.Ack, error) {
			return sendTunnelMutation(ctx, host, a.dependencies.DialControl, mutation)
		})
	if ack.Error != "" {
		return "", fmt.Errorf("edge %s refused tunnel %s: %s", host.Alias, action, safeRemoteText(ack.Error))
	}
	if err != nil {
		return "", err
	}
	return stateDir, nil
}

func sendTunnelMutation(ctx context.Context, host HostRecord, dial HostDialer, mutation tunnel.Mutation) (tunnel.Ack, error) {
	digest, err := tunnel.Verify(mutation, host.MeshIdentity)
	if err != nil {
		return tunnel.Ack{}, err
	}
	response, _, err := remoteServiceRequest(ctx, host, dial, protocol.Control{
		Type: protocol.TypeTunnelClaim, TunnelMutation: &mutation,
	}, nil)
	if err != nil {
		return tunnel.Ack{}, err
	}
	if response.Type == protocol.TypeError {
		return tunnel.Ack{}, remoteServiceResponseError(host, "tunnel mutation", response)
	}
	if response.Type != protocol.TypeTunnelClaimed || response.TunnelAck == nil {
		return tunnel.Ack{}, errors.New("edge returned an invalid tunnel acknowledgement")
	}
	ack := *response.TunnelAck
	if ack.Sequence != mutation.Sequence || ack.Digest != digest {
		return tunnel.Ack{}, errors.New("edge tunnel acknowledgement does not match the signed mutation")
	}
	return ack, nil
}

func (a *application) runLocalTunnelRelease(cmd *cobra.Command, publicName, hostAlias string) error {
	if hostAlias != "" {
		return errors.New("--local-edge cannot be combined with --host")
	}
	if err := tunnel.ValidateHostname(publicName); err != nil {
		return err
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return err
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), serviceMutationTimeout)
	defer cancel()
	response, err := daemonControlRequest(ctx, daemon.SocketPath(stateDir), protocol.Control{
		Type: protocol.TypeTunnelRecover, RequestID: requestID, TunnelName: publicName,
	})
	if err != nil {
		return err
	}
	if response.Type == protocol.TypeError {
		return daemonResponseError("local tunnel release", response.Message)
	}
	if response.Type != protocol.TypeTunnelRecovered || response.TunnelName != publicName {
		return errors.New("local edge returned an invalid tunnel release acknowledgement")
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "released %s on the local edge\n", publicName)
	return err
}
