package updatebootstrap

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shaul/mesh/internal/updateinstall"
	"golang.org/x/sys/unix"
)

func inspectInvocation(ctx context.Context, pid int) (processInvocation, error) {
	if err := ctx.Err(); err != nil {
		return processInvocation{}, fmt.Errorf("inspect daemon invocation: %w", err)
	}
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return processInvocation{}, fmt.Errorf("read same-account daemon invocation: %w", err)
	}
	return darwinInvocation(data)
}

func darwinInvocation(data []byte) (processInvocation, error) {
	if len(data) < 5 || len(data) > 1<<20 {
		return processInvocation{}, errors.New("daemon invocation has invalid process-argument bounds")
	}
	argc := int(binary.NativeEndian.Uint32(data[:4]))
	if argc < 1 || argc > 8192 {
		return processInvocation{}, errors.New("daemon invocation has invalid argument count")
	}
	_, rest, ok := bytes.Cut(data[4:], []byte{0})
	if !ok {
		return processInvocation{}, errors.New("daemon invocation has no terminated executable path")
	}
	rest = bytes.TrimLeft(rest, "\x00")
	var arguments []byte
	for range argc {
		argument, next, ok := bytes.Cut(rest, []byte{0})
		if !ok {
			return processInvocation{}, errors.New("daemon invocation has an unterminated argument")
		}
		arguments = append(arguments, argument...)
		arguments = append(arguments, 0)
		rest = next
	}
	return parseInvocation(arguments, rest)
}

func inspectDaemonService(ctx context.Context, pid int, settings *updateinstall.Settings, invocation processInvocation) (ServiceSnapshot, error) {
	directory := filepath.Join(invocation.Environment["HOME"], "Library", "LaunchAgents")
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) > 512 {
		return ServiceSnapshot{}, errors.Join(errors.New("cannot enumerate bounded same-account launch agents"), err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".plist") {
			continue
		}
		spec, matched, err := daemonLaunchAgent(ctx, pid, filepath.Join(directory, entry.Name()))
		if err != nil {
			return ServiceSnapshot{}, err
		}
		if matched {
			return snapshotLaunchAgent(ctx, settings, invocation, spec)
		}
	}
	return ServiceSnapshot{}, errors.New("no same-account launch agent owns the connected daemon PID")
}

func daemonLaunchAgent(ctx context.Context, pid int, path string) (updateinstall.ServiceSpec, bool, error) {
	command := exec.CommandContext(ctx, "plutil", "-extract", "Label", "raw", "-o", "-")
	command.Args = append(command.Args, path)
	output, err := inspectServiceCommand(ctx, command)
	if err != nil {
		return updateinstall.ServiceSpec{}, false, err
	}
	label := strings.TrimSpace(string(output))
	for _, scope := range []string{"gui", "user"} {
		domain := scope + "/" + strconv.Itoa(os.Geteuid())
		command := exec.CommandContext(ctx, "launchctl", "print")
		command.Args = append(command.Args, domain+"/"+label)
		output, err := inspectServiceCommand(ctx, command)
		if err != nil {
			continue
		}
		actual, err := launchdHelperPID(string(output))
		if err == nil && actual == pid {
			return updateinstall.ServiceSpec{Kind: installationLaunchd, Name: label, Domain: domain, ConfigPath: path}, true, nil
		}
	}
	return updateinstall.ServiceSpec{}, false, nil
}

func snapshotLaunchAgent(ctx context.Context, settings *updateinstall.Settings, invocation processInvocation, spec updateinstall.ServiceSpec) (ServiceSnapshot, error) {
	manager, err := updateinstall.NewSystemService(spec)
	if err != nil {
		return ServiceSnapshot{}, fmt.Errorf("inspect launch agent identity: %w", err)
	}
	if err := manager.Preflight(ctx); err != nil {
		return ServiceSnapshot{}, fmt.Errorf("inspect launch agent worker preservation: %w", err)
	}
	digest, err := serviceFileDigest(spec.ConfigPath)
	if err != nil {
		return ServiceSnapshot{}, err
	}
	snapshot := ServiceSnapshot{Files: map[string]string{spec.ConfigPath: digest}}
	snapshot.Definition = serviceFilesDigest(snapshot.Files)
	snapshot.Arguments, snapshot.Environment = invocationSnapshot(invocation)
	settings.Service = spec
	return snapshot, nil
}

func migrationServicePID(ctx context.Context, spec updateinstall.ServiceSpec) (int, error) {
	command := exec.CommandContext(ctx, "launchctl", "print")
	command.Args = append(command.Args, spec.Domain+"/"+spec.Name)
	var output inspectionOutput
	command.Stdout = &output
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 113 {
			return 0, nil
		}
		return 0, fmt.Errorf("inspect migration service process: %w", err)
	}
	pid, _ := launchdHelperPID(string(output.bytes))
	return pid, nil
}
