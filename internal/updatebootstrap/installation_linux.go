package updatebootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/shaul/mesh/internal/updateinstall"
)

func inspectInvocation(ctx context.Context, pid int) (processInvocation, error) {
	if err := ctx.Err(); err != nil {
		return processInvocation{}, fmt.Errorf("inspect daemon invocation: %w", err)
	}
	root, err := os.OpenRoot(filepath.Join("/proc", strconv.Itoa(pid)))
	if err != nil {
		return processInvocation{}, fmt.Errorf("open actual daemon process: %w", err)
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(".")
	if err != nil {
		return processInvocation{}, fmt.Errorf("inspect daemon process account: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) {
		return processInvocation{}, errors.New("daemon process belongs to another account")
	}
	args, err := readProcessData(root, "cmdline")
	if err != nil {
		return processInvocation{}, err
	}
	environment, err := readProcessData(root, "environ")
	if err != nil {
		return processInvocation{}, err
	}
	return parseInvocation(args, environment)
}

func readProcessData(root *os.Root, name string) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open daemon invocation data: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.Join(errors.New("read bounded daemon invocation data"), err)
	}
	return data, nil
}

func inspectDaemonService(ctx context.Context, pid int, settings *updateinstall.Settings, invocation processInvocation) (ServiceSnapshot, error) {
	command := exec.CommandContext(ctx, "systemctl", "--user", "whoami")
	command.Args = append(command.Args, strconv.Itoa(pid))
	unit, err := inspectServiceCommand(ctx, command)
	if err != nil {
		return ServiceSnapshot{}, err
	}
	name := strings.TrimSpace(string(unit))
	spec := updateinstall.ServiceSpec{Kind: installationSystemd, Name: name}
	manager, err := updateinstall.NewSystemService(spec)
	if err != nil {
		return ServiceSnapshot{}, fmt.Errorf("inspect daemon unit identity: %w", err)
	}
	main, err := systemdProperty(ctx, name, "MainPID")
	if err != nil || main != strconv.Itoa(pid) {
		return ServiceSnapshot{}, errors.Join(errors.New("service manager PID differs from the connected daemon"), err)
	}
	if err := manager.Preflight(ctx); err != nil {
		return ServiceSnapshot{}, fmt.Errorf("inspect daemon worker preservation: %w", err)
	}
	definition, err := systemdProperty(ctx, name, "FragmentPath")
	if err != nil || !filepath.IsAbs(definition) {
		return ServiceSnapshot{}, errors.Join(errors.New("daemon unit has no absolute definition path"), err)
	}
	drops, err := systemdProperty(ctx, name, "DropInPaths")
	if err != nil {
		return ServiceSnapshot{}, err
	}
	files := append([]string{definition}, strings.Fields(drops)...)
	snapshot := ServiceSnapshot{Files: make(map[string]string, len(files))}
	for _, path := range files {
		digest, err := serviceFileDigest(path)
		if err != nil {
			return snapshot, err
		}
		snapshot.Files[path] = digest
	}
	snapshot.Definition = serviceFilesDigest(snapshot.Files)
	snapshot.Arguments, snapshot.Environment = invocationSnapshot(invocation)
	settings.Service = spec
	return snapshot, nil
}

func systemdProperty(ctx context.Context, name, property string) (string, error) {
	command := exec.CommandContext(ctx, "systemctl", "--user", "show", "--value")
	command.Args = append(command.Args, "--property="+property, name)
	output, err := inspectServiceCommand(ctx, command)
	return strings.TrimSpace(string(output)), err
}

func migrationServicePID(ctx context.Context, spec updateinstall.ServiceSpec) (int, error) {
	value, err := systemdProperty(ctx, spec.Name, "MainPID")
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid < 0 {
		return 0, errors.New("daemon manager returned an invalid process identity")
	}
	return pid, nil
}
