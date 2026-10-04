package updatebootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
	"golang.org/x/sys/unix"
)

// HelperProcessProbe binds the service manager's PID to its mapped executable.
// A successful kickstart or a changed launcher cannot supply that proof.
func HelperProcessProbe(cfg updateinstall.HelperConfig) func(context.Context, updateinstall.HelperInstallation) (int, error) {
	return func(ctx context.Context, installed updateinstall.HelperInstallation) (int, error) {
		return probeHelperProcess(ctx, cfg, installed)
	}
}

func probeHelperProcess(ctx context.Context, cfg updateinstall.HelperConfig, installed updateinstall.HelperInstallation) (int, error) {
	pid, err := helperServicePID(ctx, cfg)
	if err != nil {
		return 0, err
	}
	image, err := processImage(ctx, pid)
	if err != nil {
		return 0, fmt.Errorf("inspect running helper image: %w", err)
	}
	loaded, err := os.Stat(image.Path)
	if err != nil {
		return 0, fmt.Errorf("inspect mapped helper file: %w", err)
	}
	expected, err := os.Stat(installed.Executable)
	if err != nil {
		return 0, fmt.Errorf("inspect expected helper file: %w", err)
	}
	if !os.SameFile(loaded, expected) {
		return 0, errors.New("helper service is executing another image")
	}
	if err = release.VerifyExecutable(ctx, image.Path, installed.Digest); err != nil {
		return 0, fmt.Errorf("verify mapped helper digest: %w", err)
	}
	if err = unix.Kill(pid, 0); err != nil {
		return 0, fmt.Errorf("running helper exited: %w", err)
	}
	current, err := helperServicePID(ctx, cfg)
	if err != nil || current != pid {
		return 0, errors.Join(errors.New("helper service process changed during verification"), err)
	}
	return pid, nil
}

func helperServicePID(ctx context.Context, cfg updateinstall.HelperConfig) (int, error) {
	var command *exec.Cmd
	switch cfg.Kind {
	case "launchd":
		target, err := helperLaunchdTarget(cfg.Domain)
		if err != nil {
			return 0, err
		}
		command = exec.CommandContext(ctx, "launchctl", "print")
		command.Args = append(command.Args, target)
	case "systemd":
		command = exec.CommandContext(ctx, "systemctl", "--user", "show", "mesh-update-helper.service", "--property=MainPID", "--value")
	default:
		return 0, errors.New("helper process probe requires a supported service manager")
	}
	command.WaitDelay = 100 * time.Millisecond
	output, err := command.Output()
	if err != nil {
		return 0, fmt.Errorf("inspect helper service process: %w", errors.Join(ctx.Err(), err))
	}
	if cfg.Kind == "systemd" {
		pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
		if err != nil || pid <= 0 {
			return 0, errors.New("helper service has no running process")
		}
		return pid, nil
	}
	return launchdHelperPID(string(output))
}

func helperLaunchdTarget(domain string) (string, error) {
	scope, value, ok := strings.Cut(domain, "/")
	if !ok || scope != "gui" && scope != "user" {
		return "", errors.New("helper process probe requires a user launchd domain")
	}
	uid, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return "", fmt.Errorf("read helper launchd user identity: %w", err)
	}
	return fmt.Sprintf("%s/%d/dev.shaulavo.mesh-update-helper", scope, uid), nil
}

func launchdHelperPID(output string) (int, error) {
	values := make(map[string]string, 2)
	for _, line := range strings.Split(output, "\n") {
		// launchctl indents job fields by one tab and nested section fields by more.
		if len(line) < 2 || line[0] != '\t' || line[1] == '\t' || line[1] == ' ' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "state" && fields[0] != "pid" {
			continue
		}
		if len(fields) != 3 || fields[1] != "=" {
			return 0, errors.New("helper service process fields are malformed")
		}
		if _, seen := values[fields[0]]; seen {
			return 0, errors.New("helper service process is ambiguous")
		}
		values[fields[0]] = fields[2]
	}
	pid, err := strconv.ParseUint(values["pid"], 10, strconv.IntSize-1)
	if values["state"] != "running" || err != nil || pid == 0 {
		return 0, errors.New("helper service has no running process")
	}
	return int(pid), nil
}
