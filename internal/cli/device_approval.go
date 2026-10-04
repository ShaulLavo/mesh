package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

type checkedApproval struct {
	Account     string
	StateDir    string
	Destination string
	Source      string
	Apply       bool
	AllowRoot   bool
}

type approvalReceipt struct {
	Destination string `json:"destination"`
	Source      string `json:"source"`
	Approved    bool   `json:"approved"`
	Root        bool   `json:"root"`
}

func checkedApprovalCommand() *cobra.Command {
	var request checkedApproval
	var check bool
	command := &cobra.Command{Use: "approve-checked ID", Short: "Check the local daemon account, state and pinned identity before approval", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if check == request.Apply {
				return errors.New("select exactly one of --check or --yes")
			}
			request.Source = args[0]
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			result, err := approveChecked(ctx, request)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}}
	command.Flags().StringVar(&request.Account, "account", "", "expected local daemon OS account")
	command.Flags().StringVar(&request.StateDir, "state-dir", "", "existing canonical daemon state directory")
	command.Flags().StringVar(&request.Destination, "destination", "", "expected destination Mesh identity pin")
	command.Flags().BoolVar(&check, "check", false, "check existing state without writing a grant")
	command.Flags().BoolVar(&request.Apply, "yes", false, "grant the source full control and SSH access")
	command.Flags().BoolVar(&request.AllowRoot, "allow-root", false, "acknowledge that approval grants root access")
	return command
}

func approveChecked(ctx context.Context, request checkedApproval) (approvalReceipt, error) {
	result := approvalReceipt{Destination: request.Destination, Source: request.Source, Root: os.Geteuid() == 0}
	if err := validateCheckedApproval(request); err != nil {
		return result, err
	}
	directory, err := existingApprovalDirectory(request.StateDir)
	if err != nil {
		return result, err
	}
	if err := checkApprovalBinding(ctx, request); err != nil {
		return result, err
	}
	grants, err := identity.DeviceGrants(filepath.Join(request.StateDir, "authorized_keys"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("read existing device grants: %w", err)
	}
	result.Approved = containsDeviceGrant(grants, request.Source)
	if !request.Apply {
		return result, nil
	}
	current, err := existingApprovalDirectory(request.StateDir)
	if err != nil || !os.SameFile(directory, current) {
		return result, errors.New("daemon state directory changed before approval")
	}
	if err := checkApprovalBinding(ctx, request); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("approval canceled: %w", err)
	}
	if err := identity.ApproveDevice(request.StateDir, request.Source); err != nil {
		return result, fmt.Errorf("approve source device: %w", err)
	}
	grants, err = identity.DeviceGrants(filepath.Join(request.StateDir, "authorized_keys"))
	if err != nil {
		return result, fmt.Errorf("read back device approval: %w", err)
	}
	result.Approved = containsDeviceGrant(grants, request.Source)
	if !result.Approved {
		return result, errors.New("source grant missing after approval")
	}
	return result, nil
}

func containsDeviceGrant(grants []identity.DeviceGrant, source string) bool {
	for _, grant := range grants {
		if grant.Identity == source {
			return true
		}
	}
	return false
}

func validateCheckedApproval(request checkedApproval) error {
	account, err := user.Current()
	if err != nil {
		return fmt.Errorf("read local account: %w", err)
	}
	if request.Account == "" || account.Username != request.Account || account.Uid != fmt.Sprint(os.Geteuid()) || os.Getuid() != os.Geteuid() {
		return errors.New("local account does not match the expected daemon account")
	}
	if request.Apply && os.Geteuid() == 0 && !request.AllowRoot {
		return errors.New("approving this device grants root access; add --allow-root to acknowledge it")
	}
	for _, id := range []string{request.Destination, request.Source} {
		if _, err := identity.IdentityKey(id); err != nil {
			return fmt.Errorf("validate checked approval identity: %w", err)
		}
	}
	return nil
}

func existingApprovalDirectory(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("daemon state directory must be an existing canonical absolute path")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve existing daemon state directory: %w", err)
	}
	if canonical != path {
		return nil, errors.New("daemon state directory must use its canonical path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect existing daemon state directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0022 != 0 || int64(stat.Uid) != int64(os.Geteuid()) {
		return nil, errors.New("daemon state directory must be owned by the local account with safe permissions")
	}
	return info, nil
}

func checkApprovalBinding(ctx context.Context, request checkedApproval) error {
	host, err := identity.LoadOwned(request.StateDir)
	if err != nil {
		return fmt.Errorf("read existing destination identity: %w", err)
	}
	if host.ID != request.Destination {
		return errors.New("existing destination identity does not match the saved pin")
	}
	stream, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(request.StateDir, "daemon.sock"))
	if err != nil {
		return fmt.Errorf("connect to existing local daemon: %w", err)
	}
	defer func() { _ = stream.Close() }()
	pid, err := approvalPeerPID(stream)
	if err != nil {
		return fmt.Errorf("observe local daemon socket peer: %w", err)
	}
	if err := approvalProcessState(ctx, pid, request.StateDir); err != nil {
		return err
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		return fmt.Errorf("adapt local daemon stream: %w", err)
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID})
	if err != nil {
		return err
	}
	if response.Type != protocol.TypeHostInfoResult || response.Host == nil || response.Host.ID != host.ID || response.Host.MeshIdentity != request.Destination {
		return errors.New("local daemon identity does not match the existing state and saved pin")
	}
	return nil
}

func approvalProcessState(ctx context.Context, pid int32, expected string) error {
	peer, err := process.NewProcessWithContext(ctx, pid)
	if err != nil {
		return fmt.Errorf("observe local daemon process: %w", err)
	}
	uids, err := peer.UidsWithContext(ctx)
	if err != nil || len(uids) == 0 {
		return fmt.Errorf("local daemon account binding unavailable: %w", errors.Join(err, errors.New("no observed process account")))
	}
	for _, uid := range uids {
		if int64(uid) != int64(os.Geteuid()) {
			return errors.New("local daemon process belongs to a different account")
		}
	}
	environment, err := peer.EnvironWithContext(ctx)
	if err != nil {
		return fmt.Errorf("local daemon state binding unavailable: %w", err)
	}
	actual, err := approvalEnvironmentState(environment)
	if err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(actual)
	if err != nil || canonical != expected {
		return errors.New("local daemon process state directory does not match the selected state")
	}
	return nil
}

func approvalEnvironmentState(environment []string) (string, error) {
	values := make(map[string]string)
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || (key != "MESH_STATE_DIR" && key != "XDG_STATE_HOME" && key != "HOME") {
			continue
		}
		if _, exists := values[key]; exists {
			return "", errors.New("local daemon process state environment is ambiguous")
		}
		values[key] = value
	}
	state := values["MESH_STATE_DIR"]
	if state == "" && values["XDG_STATE_HOME"] != "" {
		state = filepath.Join(values["XDG_STATE_HOME"], "mesh")
	}
	if state == "" && values["HOME"] != "" {
		state = filepath.Join(values["HOME"], ".local", "state", "mesh")
	}
	if !filepath.IsAbs(state) {
		return "", errors.New("local daemon process state binding unavailable; an absolute state home must be observable")
	}
	return filepath.Clean(state), nil
}
