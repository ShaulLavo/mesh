package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
)

type deviceIdentity struct {
	ID          string `json:"id"`
	Account     string `json:"account"`
	Fingerprint string `json:"fingerprint"`
}

func deviceCommand() *cobra.Command {
	command := &cobra.Command{Use: "device", Short: "Approve device keys for this daemon's OS account"}
	command.AddCommand(deviceIdentityCommand(), deviceGrantCommand(true), deviceGrantCommand(false))
	return command
}

func deviceIdentityCommand() *cobra.Command {
	var structured bool
	command := &cobra.Command{Use: "identity", Short: "Show this device's Mesh key and daemon account", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stateDir, err := paths.StateDir()
			if err != nil {
				return fmt.Errorf("device command: %w", err)
			}
			host, _, err := identity.LoadOrCreate(stateDir)
			if err != nil {
				return fmt.Errorf("device command: %w", err)
			}
			account, err := user.Current()
			if err != nil {
				return fmt.Errorf("read device account: %w", err)
			}
			key, err := ssh.NewPublicKey(host.PublicKey)
			if err != nil {
				return fmt.Errorf("encode device fingerprint: %w", err)
			}
			result := deviceIdentity{ID: host.ID, Account: account.Username, Fingerprint: ssh.FingerprintSHA256(key)}
			if structured {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\naccount %s\nfingerprint %s\n", result.ID, result.Account, result.Fingerprint)
			if err != nil {
				return fmt.Errorf("write device identity: %w", err)
			}
			return nil
		}}
	command.Flags().BoolVar(&structured, "json", false, "write identity, account and fingerprint as JSON")
	return command
}

func deviceGrantCommand(approve bool) *cobra.Command {
	name, description := "revoke", "Remove a device's session, service and SSH grant"
	if approve {
		name, description = "approve", "Grant a device this daemon account's control and SSH access"
	}
	var allowRoot bool
	command := &cobra.Command{Use: name + " ID", Short: description, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if approve && os.Geteuid() == 0 && !allowRoot {
				return errors.New("approving this device grants root access; add --allow-root to acknowledge it")
			}
			stateDir, err := paths.StateDir()
			if err != nil {
				return fmt.Errorf("device command: %w", err)
			}
			mutate := identity.RevokeDevice
			if approve {
				mutate = identity.ApproveDevice
			}
			if err := mutate(stateDir, args[0]); err != nil {
				return fmt.Errorf("device command: %w", err)
			}
			return writeDeviceGrant(cmd, name, args[0])
		}}
	if approve {
		command.Flags().BoolVar(&allowRoot, "allow-root", false, "acknowledge that approval grants root access")
	}
	return command
}

func writeDeviceGrant(cmd *cobra.Command, name, id string) error {
	key, err := identity.IdentityKey(id)
	if err != nil {
		return fmt.Errorf("encode device identity: %w", err)
	}
	public, err := ssh.NewPublicKey(key)
	if err != nil {
		return fmt.Errorf("encode device fingerprint: %w", err)
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", name, ssh.FingerprintSHA256(public)); err != nil {
		return fmt.Errorf("write device grant result: %w", err)
	}
	return nil
}
