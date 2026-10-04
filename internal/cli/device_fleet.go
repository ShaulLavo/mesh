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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
)

const fleetInputLimit = 64 << 10

type fleetAdmin struct {
	Target   string `json:"target"`
	Account  string `json:"account"`
	StateDir string `json:"stateDir"`
	Binary   string `json:"binary"`
}

type fleetDestination struct {
	Label string
	Host  HostRecord
	Admin fleetAdmin
}

func approveFleetCommand() *cobra.Command {
	var mapPath string
	var check, apply, allowRoot bool
	command := &cobra.Command{Use: "approve-fleet HOST…", Short: "Approve this device on explicitly selected pinned hosts using system SSH", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if check == apply {
				return errors.New("select exactly one of --check or --yes")
			}
			destinations, err := selectedFleet(args, mapPath)
			if err != nil {
				return err
			}
			state, err := paths.StateDirPath()
			if err != nil {
				return fmt.Errorf("resolve existing source state: %w", err)
			}
			if _, err := existingApprovalDirectory(state); err != nil {
				return err
			}
			source, err := identity.LoadOwned(state)
			if err != nil {
				return fmt.Errorf("read existing source identity: %w", err)
			}
			return runApprovalFleet(cmd.Context(), cmd.OutOrStdout(), destinations, state, source.ID, apply, allowRoot)
		}}
	command.Flags().StringVar(&mapPath, "admin-map", "", "JSON map of selected host labels to explicit SSH target, account, stateDir and staged binary")
	command.Flags().BoolVar(&check, "check", false, "preview each selected destination without writing grants")
	command.Flags().BoolVar(&apply, "yes", false, "grant this source full control and SSH access on the selected destinations")
	command.Flags().BoolVar(&allowRoot, "allow-root", false, "acknowledge selected destinations that grant root access")
	return command
}

func selectedFleet(labels []string, mapPath string) ([]fleetDestination, error) {
	if len(labels) > maximumConfiguredHosts {
		return nil, errors.New("selected fleet exceeds host limit")
	}
	administration, err := readFleetMap(mapPath)
	if err != nil {
		return nil, err
	}
	hosts, err := LoadHosts()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	destinations := make([]fleetDestination, 0, len(labels))
	for _, label := range labels {
		target, err := ResolveArgument(label, hosts)
		if err != nil || target.Host == nil {
			return nil, fmt.Errorf("select configured pinned host %q: %w", label, errors.Join(err, errors.New("an existing configured host is required")))
		}
		if seen[target.Host.ID] {
			return nil, fmt.Errorf("host %q was selected more than once", label)
		}
		seen[target.Host.ID] = true
		if _, err := identity.IdentityKey(target.Host.MeshIdentity); err != nil {
			return nil, fmt.Errorf("host %q has an invalid saved identity pin: %w", label, err)
		}
		admin, exists := administration[label]
		if !exists {
			return nil, fmt.Errorf("host %q has no explicit administration mapping", label)
		}
		if err := validateFleetAdmin(admin); err != nil {
			return nil, fmt.Errorf("host %q administration: %w", label, err)
		}
		destinations = append(destinations, fleetDestination{Label: label, Host: *target.Host, Admin: admin})
	}
	return destinations, nil
}

func readFleetMap(path string) (map[string]fleetAdmin, error) {
	if path == "" {
		return nil, errors.New("provide --admin-map with explicit destination administration targets")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // held nonblocking descriptor for the caller-selected regular administration file
	if err != nil {
		return nil, fmt.Errorf("open administration map: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect administration map: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("administration map must be a regular file")
	}
	var result map[string]fleetAdmin
	if err := decodeFleetJSON(io.LimitReader(file, fleetInputLimit+1), &result); err != nil {
		return nil, fmt.Errorf("read administration map: %w", err)
	}
	if len(result) == 0 || len(result) > maximumConfiguredHosts {
		return nil, errors.New("administration map must contain between 1 and 256 destinations")
	}
	return result, nil
}

func decodeFleetJSON(reader io.Reader, result any) error {
	contents, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read enrollment JSON: %w", err)
	}
	if len(contents) > fleetInputLimit {
		return errors.New("enrollment JSON exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("decode enrollment JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("enrollment JSON has trailing data")
	}
	return nil
}

func validateFleetAdmin(admin fleetAdmin) error {
	if admin.Target == "" || strings.HasPrefix(admin.Target, "-") || strings.ContainsAny(admin.Target, " \t\r\n\x00") {
		return errors.New("provide an explicit system SSH target or Host alias")
	}
	if admin.Account == "" || strings.ContainsAny(admin.Account, "\r\n\x00") {
		return errors.New("provide the expected destination OS account")
	}
	for _, path := range []string{admin.StateDir, admin.Binary} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\r\n\x00") {
			return errors.New("stateDir and staged binary must be explicit absolute paths")
		}
	}
	return nil
}

func runApprovalFleet(ctx context.Context, output io.Writer, destinations []fleetDestination, state, source string, apply, allowRoot bool) error {
	for _, destination := range destinations {
		receipt, err := invokeFleetApproval(ctx, destination, source, false, allowRoot)
		if err != nil {
			return fleetFailure(output, destinations, 0, destination.Label, "preflight failed", err)
		}
		if apply && receipt.Root && !allowRoot {
			return fleetFailure(output, destinations, 0, destination.Label, "preflight failed", errors.New("selected destination grants root access; add --allow-root to acknowledge it"))
		}
		status := "approval available"
		if receipt.Approved {
			status = "already approved"
		}
		if receipt.Root {
			status += "; grants root access"
		}
		if _, err := fmt.Fprintf(output, "%s: %s\n", destination.Label, status); err != nil {
			return fmt.Errorf("write fleet preflight: %w", err)
		}
	}
	if !apply {
		return nil
	}
	return applyApprovalFleet(ctx, output, destinations, state, source, allowRoot)
}

func applyApprovalFleet(ctx context.Context, output io.Writer, destinations []fleetDestination, state, source string, allowRoot bool) error {
	for index, destination := range destinations {
		if err := revalidateFleetSource(state, source, destination); err != nil {
			return fleetFailure(output, destinations, index, destination.Label, "approval refused", err)
		}
		if _, err := invokeFleetApproval(ctx, destination, source, true, allowRoot); err != nil {
			return fleetFailure(output, destinations, index, destination.Label, "approval outcome unknown", err)
		}
		if _, err := fmt.Fprintf(output, "%s: approved\n", destination.Label); err != nil {
			return fmt.Errorf("approval completed for %s; write result: %w", destination.Label, err)
		}
	}
	return nil
}

func revalidateFleetSource(state, source string, destination fleetDestination) error {
	if _, err := existingApprovalDirectory(state); err != nil {
		return err
	}
	host, err := identity.LoadOwned(state)
	if err != nil || host.ID != source {
		return errors.New("source identity changed before approval")
	}
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	target, err := ResolveArgument(destination.Label, hosts)
	if err != nil || target.Host == nil || target.Host.ID != destination.Host.ID || target.Host.MeshIdentity != destination.Host.MeshIdentity {
		return errors.New("selected destination or saved pin changed before approval")
	}
	return nil
}

func fleetFailure(output io.Writer, destinations []fleetDestination, completed int, failed, status string, cause error) error {
	if _, err := fmt.Fprintf(output, "%s: %s; %d of %d destinations approved\n", failed, status, completed, len(destinations)); err != nil {
		return errors.Join(cause, err)
	}
	for _, destination := range destinations[completed:] {
		if destination.Label != failed {
			if _, err := fmt.Fprintf(output, "%s: pending\n", destination.Label); err != nil {
				return errors.Join(cause, err)
			}
		}
	}
	return fmt.Errorf("host %s %s: %w", failed, status, cause)
}

func invokeFleetApproval(ctx context.Context, destination fleetDestination, source string, apply, allowRoot bool) (approvalReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := []string{destination.Admin.Binary, "device", "approve-checked", "--account", destination.Admin.Account,
		"--state-dir", destination.Admin.StateDir, "--destination", destination.Host.MeshIdentity}
	mode := "--check"
	if apply {
		mode = "--yes"
	}
	args = append(args, mode)
	if allowRoot {
		args = append(args, "--allow-root")
	}
	args = append(args, "--", source)
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	command := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=5", "--", destination.Admin.Target, strings.Join(quoted, " ")) //nolint:gosec // explicit system SSH authority; fixed flags and shell-quoted staged binary arguments
	command.WaitDelay = time.Second
	var stdout, stderr enrollmentOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return approvalReceipt{}, fmt.Errorf("system SSH checked approval failed: %w; %s", errors.Join(ctx.Err(), err), safeRemoteText(stderr.String()))
	}
	var receipt approvalReceipt
	if err := decodeFleetJSON(strings.NewReader(stdout.String()), &receipt); err != nil {
		return receipt, fmt.Errorf("read checked approval receipt: %w", err)
	}
	if receipt.Destination != destination.Host.MeshIdentity || receipt.Source != source || (apply && !receipt.Approved) {
		return receipt, errors.New("checked approval receipt does not match the requested destination, source and grant")
	}
	return receipt, nil
}

type enrollmentOutput struct{ bytes.Buffer }

func (output *enrollmentOutput) Write(data []byte) (int, error) {
	if output.Len()+len(data) > fleetInputLimit {
		return 0, errors.New("checked approval output exceeds size limit")
	}
	count, err := output.Buffer.Write(data)
	if err != nil {
		return count, fmt.Errorf("capture checked approval output: %w", err)
	}
	return count, nil
}
