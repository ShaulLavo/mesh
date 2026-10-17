package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/fleetshare"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
)

func fleetDirectory() (string, error) {
	path, err := ConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

func fleetDeviceIdentities() ([]string, error) {
	directory, err := fleetDirectory()
	if err != nil {
		return nil, err
	}
	peers, err := fleetshare.Peers(directory)
	if err != nil {
		return nil, fmt.Errorf("read trusted connection-sharing devices: %w", err)
	}
	devices := make([]string, len(peers))
	for i, peer := range peers {
		devices[i] = peer.Identity
	}
	return devices, nil
}

func configuredFleetService(state string) (*fleetshare.Service, error) {
	directory, err := fleetDirectory()
	if err != nil {
		return nil, err
	}
	return &fleetshare.Service{Directory: directory, StateDir: state, LocalHosts: localFleetHosts}, nil
}

func localFleetHosts(context.Context) ([]protocol.FleetHost, error) {
	config, err := loadHostConfig()
	if err != nil {
		return nil, err
	}
	hosts := make([]protocol.FleetHost, len(config.Hosts))
	for i, host := range config.Hosts {
		hosts[i] = protocol.FleetHost{ID: host.ID, MeshIdentity: host.MeshIdentity, TailscaleName: host.TailscaleName, Addresses: host.Addresses, Endpoint: host.Endpoint}
	}
	return hosts, nil
}

func withSharedConnections(local []HostRecord, shared []protocol.FleetHost) ([]HostRecord, error) {
	if len(shared) == 0 {
		return local, nil
	}
	state, err := paths.StateDirPath()
	if err != nil {
		return nil, fmt.Errorf("locate shared connection identity: %w", err)
	}
	owner, err := existingLocalIdentity(state)
	if err != nil {
		return nil, err
	}
	return mergeSharedHosts(local, shared, owner.ID)
}

func mergeSharedHosts(local []HostRecord, shared []protocol.FleetHost, own string) ([]HostRecord, error) {
	seen := make(map[string]bool, len(local))
	for _, host := range local {
		seen[host.ID] = true
	}
	for _, host := range shared {
		if seen[host.ID] || host.ID == own {
			continue
		}
		seen[host.ID] = true
		local = append(local, HostRecord{ID: host.ID, MeshIdentity: host.MeshIdentity, TailscaleName: host.TailscaleName, Addresses: host.Addresses, Endpoint: host.Endpoint})
	}
	if len(local) > maximumConfiguredHosts {
		return nil, fmt.Errorf("local and shared connections exceed %d hosts", maximumConfiguredHosts)
	}
	return local, nil
}

func syncFleetHosts(ctx context.Context) error {
	directory, err := fleetDirectory()
	if err != nil {
		return err
	}
	state, err := paths.StateDir()
	if err != nil {
		return fmt.Errorf("locate connection-sharing state: %w", err)
	}
	if err := fleetshare.Sync(ctx, directory, state); err != nil {
		return fmt.Errorf("refresh shared connections: %w", err)
	}
	return nil
}

func (a *application) fleetCommand() *cobra.Command {
	command := &cobra.Command{Use: "fleet", Short: "Share connections with explicitly trusted devices"}
	command.AddCommand(a.fleetTrustCommand(true), a.fleetTrustCommand(false))
	command.AddCommand(&cobra.Command{Use: "sync", Short: "Fetch connections from trusted devices", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), remoteConnectTimeout)
			defer cancel()
			if err := syncFleetHosts(ctx); err != nil {
				return err
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "shared connections refreshed")
			if err != nil {
				return fmt.Errorf("write connection-sharing result: %w", err)
			}
			return nil
		}})
	return command
}

func (a *application) fleetTrustCommand(approve bool) *cobra.Command {
	name, description := "untrust", "Stop sharing connections with this device"
	if approve {
		name, description = "trust", "Allow this device to share connections and access newly added servers"
	}
	var yes bool
	command := &cobra.Command{Use: name + " HOST", Short: description, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return errors.New("connection sharing changes server access; add --yes to confirm the selected device")
			}
			return a.changeFleetTrust(cmd, args[0], approve)
		}}
	command.Flags().BoolVar(&yes, "yes", false, "approve this device's connection-sharing policy")
	return command
}

func (a *application) changeFleetTrust(cmd *cobra.Command, value string, approve bool) error {
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	target, err := resolveArgumentWithRefresh(cmd.Context(), value, hosts, a.dependencies.DialControl)
	if err != nil || target.Host == nil {
		return errors.Join(err, errors.New("select an existing pinned device by name or exact host ID"))
	}
	directory, err := fleetDirectory()
	if err != nil {
		return err
	}
	if !approve {
		if err := fleetshare.Remove(directory, target.Host.MeshIdentity); err != nil {
			return fmt.Errorf("stop sharing with device: %w", err)
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "stopped sharing connections with %s\n", a.privacy.Value("host", HostLabel(*target.Host)))
		if err != nil {
			return fmt.Errorf("write connection-sharing result: %w", err)
		}
		return nil
	}
	return a.trustFleetHost(cmd, directory, *target.Host)
}

func (a *application) trustFleetHost(cmd *cobra.Command, directory string, host HostRecord) error {
	if _, err := identity.IdentityKey(host.MeshIdentity); err != nil || host.ID != host.MeshIdentity {
		return errors.New("connection-sharing device has an invalid saved identity pin")
	}
	if host.ID == localHostID() {
		return errors.New("select another device to share connections with")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), remoteConnectTimeout)
	defer cancel()
	conn, info, err := openVerifiedHostInfo(ctx, host, a.dependencies.DialControl)
	if err != nil {
		return err
	}
	_ = conn.Close()
	host.MachineName = info.MachineName
	if err := fleetshare.Enroll(directory, fleetshare.Peer{Identity: host.ID, Endpoint: host.Endpoint}); err != nil {
		return fmt.Errorf("trust connection-sharing device: %w", err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "sharing connections with %s; newly added servers will approve this device\nRun mesh fleet trust %s --yes on that device to share in both directions.\n", a.privacy.Value("host", HostLabel(host)), a.privacy.Value("host-id", localHostID()))
	if err != nil {
		return fmt.Errorf("write connection-sharing result: %w", err)
	}
	return nil
}
