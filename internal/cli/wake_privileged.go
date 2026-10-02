package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/wake"
)

const wakeArmTimeout = 20 * time.Second

// armWake configures this host's own NIC, reporting whether a wired interface
// was found at all. Wake permission is only ever granted for hardware it has
// armed, so that a grant cannot promise more than the machine will do.
func armWake(ctx context.Context, output io.Writer, arm bool) (wake.ArmState, bool, error) {
	nic, err := wake.Discover(ctx)
	if err != nil {
		// No wired NIC. The daemon reaches the same conclusion and says so;
		// there is nothing to arm and nothing to warn about.
		return wake.ArmState{}, false, nil
	}
	state, err := armNIC(ctx, output, nic.MAC, arm)
	return state, true, err
}

// armNIC runs the one step that needs root. The kernel gates ethtool wake state
// behind CAP_NET_ADMIN and the daemon runs unprivileged, so mesh re-runs itself
// under sudo. sudo owns the password prompt; mesh never handles the secret.
func armNIC(ctx context.Context, output io.Writer, mac string, arm bool) (wake.ArmState, error) {
	if os.Geteuid() == 0 {
		if arm {
			return wake.Arm(ctx, mac)
		}
		return wake.Disarm(ctx, mac)
	}
	self, err := os.Executable()
	if err != nil {
		return wake.ArmState{}, fmt.Errorf("locate the running mesh binary: %w", err)
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return wake.ArmState{}, errors.New("configuring wake needs root, and sudo is not installed; re-run this command as root")
	}
	verb := "disarm"
	if arm {
		verb = "arm"
	}
	if _, err := fmt.Fprintf(output, "Configuring wake on %s needs root.\n", mac); err != nil {
		return wake.ArmState{}, err
	}
	stdout := &bytes.Buffer{}
	command := exec.CommandContext(ctx, sudo, "--", self, "wake", verb, "--mac", mac, "--json") //nolint:gosec // self is this process's own executable and mac is a validated MAC
	command.Stdin = os.Stdin
	command.Stdout = stdout
	command.Stderr = output
	if err := command.Run(); err != nil {
		return wake.ArmState{}, fmt.Errorf("%s wake on %s: %w", verb, mac, err)
	}
	var state wake.ArmState
	if err := json.Unmarshal(stdout.Bytes(), &state); err != nil {
		return wake.ArmState{}, fmt.Errorf("read the result of %s wake: %w", verb, err)
	}
	return state, nil
}

// armWakeCommand is the privileged half of "wake allow" and "wake deny". It is
// hidden because operators run the visible commands; this is what those re-run
// under sudo.
func armWakeCommand(arm bool) *cobra.Command {
	verb, short := "arm", "Enable wake on magic packet for this host's wired NIC"
	if !arm {
		verb, short = "disarm", "Disable wake on magic packet for this host's wired NIC"
	}
	var mac string
	var emitJSON bool
	command := &cobra.Command{
		Use: verb, Short: short, Args: cobra.NoArgs, Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if mac == "" {
				return errors.New("--mac is required")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), wakeArmTimeout)
			defer cancel()
			state, err := wake.Disarm(ctx, mac)
			if arm {
				state, err = wake.Arm(ctx, mac)
			}
			if err != nil {
				return err
			}
			if emitJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(state)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s armed=%t persisted=%t\n", state.Device, state.Armed, state.Persisted)
			return err
		},
	}
	command.Flags().StringVar(&mac, "mac", "", "MAC address of the wired interface to configure")
	command.Flags().BoolVar(&emitJSON, "json", false, "Print the resulting NIC state as JSON")
	return command
}

// describeWakeState reports anything left between an armed NIC and a machine
// that actually wakes, rather than leaving it to be found out at 2am.
func describeWakeState(output io.Writer, state wake.ArmState) error {
	if !state.Persisted {
		if _, err := fmt.Fprintf(output, "Warning: wake on %s is set for this boot only; it was not made persistent.\n", state.Device); err != nil {
			return err
		}
	}
	if state.Firmware == "disabled" {
		if _, err := fmt.Fprintf(output, "Warning: firmware lists %s as a disabled wake source, so this host ignores the packet once it sleeps.\nEnable Wake on LAN (often \"Power On By PCI-E\") in the BIOS setup.\n", state.Device); err != nil {
			return err
		}
	}
	return nil
}
