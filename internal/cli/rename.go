package cli

import (
	"context"
	"fmt"
	"github.com/shaul/mesh/internal/paths"
	"path/filepath"

	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/spf13/cobra"
)

func (a *application) renameCommand() *cobra.Command {
	return &cobra.Command{
		Use: "rename host new-name", Aliases: []string{"mv"}, Short: "Change the destination machine's name",
		Args: exactArgs(2, "a machine name or exact host ID and its new name", "mesh rename HOST_ID work-pc"),
		RunE: func(cmd *cobra.Command, args []string) error {
			hosts, err := machineTargetHosts(cmd.Context())
			if err != nil {
				return err
			}
			host, err := resolveHostTarget(hosts, args[0])
			if err != nil {
				return err
			}
			dial := a.dependencies.DialControl
			if host.local {
				stateDir, err := paths.StateDir()
				if err != nil {
					return fmt.Errorf("locate own rename state: %w", err)
				}
				dial = dashboardControlDialer(host.ID, filepath.Join(stateDir, "daemon.sock"), dial)
			}
			claim, err := RenameHost(cmd.Context(), host, args[1], dial)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "renamed %s to %s (revision %d)\nOther devices adopt this name when they reconnect. An unseen competing claim can still cause a conflict.\n", a.privacy.Value("host-id", claim.ID), a.privacy.Value("host", claim.MachineName), claim.Revision)
			return err
		},
	}
}

// RenameHost pins the owner and uses its current revision on the same authenticated connection.
func RenameHost(ctx context.Context, host HostRecord, value string, dial HostDialer) (machinename.Claim, error) {
	name, err := machinename.Normalize(value)
	if err != nil {
		return machinename.Claim{}, fmt.Errorf("normalize machine name: %w", err)
	}
	hosts, err := LoadHosts()
	if err != nil {
		return machinename.Claim{}, err
	}
	conn, info, err := openVerifiedHostInfo(ctx, host, dial)
	if err != nil {
		return machinename.Claim{}, err
	}
	defer conn.Close() //nolint:errcheck // control descriptor cleanup
	owner, err := targetOwnerClaim(ctx, host, info)
	if err != nil {
		return machinename.Claim{}, err
	}
	hosts = withOwnerClaim(hosts, owner)
	for _, other := range hosts {
		if other.ID != host.ID && other.MachineName == name {
			return machinename.Claim{}, fmt.Errorf("machine name %q is already claimed by %s; choose another name", name, other.ID)
		}
	}
	if info.NameRevision == 0 {
		return machinename.Claim{}, fmt.Errorf("destination %s has no naming declaration; update that destination", host.ID)
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return machinename.Claim{}, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{RequestID: requestID, Type: protocol.TypeHostRename, Rename: &protocol.HostRename{TargetID: host.ID, MachineName: name, ExpectedRevision: info.NameRevision}})
	if err != nil {
		return machinename.Claim{}, err
	}
	if response.Type != protocol.TypeHostRenamed || response.Host == nil {
		return machinename.Claim{}, fmt.Errorf("destination %s rejected name change: %s", host.ID, safeRemoteText(response.Message))
	}
	if err := rememberHostName(ctx, host, *response.Host); err != nil {
		return machinename.Claim{}, err
	}
	claim := declaredName(*response.Host)
	if claim.MachineName != name {
		return machinename.Claim{}, fmt.Errorf("destination %s returned another name after rename", host.ID)
	}
	return claim, nil
}
