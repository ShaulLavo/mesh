package updateinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type SystemService struct{ Spec ServiceSpec }

var serviceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]*$`)
var launchDomainPattern = regexp.MustCompile(`^(gui|user)/[0-9]+$`)

func NewSystemService(spec ServiceSpec) (*SystemService, error) {
	if !serviceNamePattern.MatchString(spec.Name) {
		return nil, errors.New("invalid update daemon service name")
	}
	if spec.Kind != "systemd" && spec.Kind != "launchd" {
		return nil, errors.New("update daemon service must use systemd or launchd")
	}
	if spec.Kind == "launchd" && (!launchDomainPattern.MatchString(spec.Domain) || !filepath.IsAbs(spec.ConfigPath)) {
		return nil, errors.New("launchd requires an explicit user domain and existing plist path")
	}
	return &SystemService{Spec: spec}, nil
}

func (s *SystemService) Stop(ctx context.Context) error {
	if err := s.Preflight(ctx); err != nil {
		return err
	}
	if s.Spec.Kind == "launchd" {
		return s.stopLaunchd(ctx)
	}
	_, err := runCommand(ctx, "systemctl", "--user", "stop", s.Spec.Name)
	return err
}

func (s *SystemService) Preflight(ctx context.Context) error {
	if s.Spec.Kind == "launchd" {
		preserve, err := runCommand(ctx, "plutil", "-extract", "AbandonProcessGroup", "raw", "-o", "-", s.Spec.ConfigPath)
		if err != nil {
			return err
		}
		if strings.TrimSpace(preserve) != "true" {
			return errors.New("daemon launchd service must set AbandonProcessGroup=true")
		}
		return nil
	}
	mode, err := runCommand(ctx, "systemctl", "--user", "show", s.Spec.Name, "--property=KillMode", "--value")
	if err != nil {
		return err
	}
	if strings.TrimSpace(mode) != "process" {
		return errors.New("daemon service must use KillMode=process to preserve session workers")
	}
	return nil
}

func (s *SystemService) Start(ctx context.Context) error {
	if s.Spec.Kind == "systemd" {
		_, err := runCommand(ctx, "systemctl", "--user", "start", s.Spec.Name)
		return err
	}
	loaded, err := s.launchdLoaded(ctx)
	if err != nil {
		return err
	}
	if loaded {
		return nil
	}
	_, err = runCommand(ctx, "launchctl", "bootstrap", s.Spec.Domain, s.Spec.ConfigPath)
	return err
}

func (s *SystemService) stopLaunchd(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	target := s.Spec.Domain + "/" + s.Spec.Name
	loaded, err := s.launchdLoaded(ctx)
	if ctx.Err() != nil {
		return fmt.Errorf("inspect launchd service %s: %w", target, ctx.Err())
	}
	if err != nil {
		return err
	}
	if !loaded {
		return nil
	}
	if _, err := runCommand(ctx, "launchctl", "bootout", target); err != nil {
		return err
	}
	// bootout can return while the outgoing job remains visible to Start.
	for {
		loaded, err := s.launchdLoaded(ctx)
		if ctx.Err() != nil {
			return fmt.Errorf("wait for launchd service %s removal: %w", target, ctx.Err())
		}
		if err != nil {
			return err
		}
		if !loaded {
			return nil
		}
		if err = waitContext(ctx, 100*time.Millisecond); err != nil {
			return fmt.Errorf("wait for launchd service %s removal: %w", target, err)
		}
	}
}

func (s *SystemService) launchdLoaded(ctx context.Context) (bool, error) {
	output, err := runCommand(ctx, "launchctl", "print", s.Spec.Domain+"/"+s.Spec.Name)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	missing := fmt.Sprintf("Could not find service %q in domain", s.Spec.Name)
	if errors.As(err, &exit) && exit.ExitCode() == 113 && strings.Contains(output, missing) {
		return false, nil
	}
	return false, err
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // callers select fixed service tools or a verified helper executable and structured arguments
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func DefaultServiceSpec() (ServiceSpec, error) {
	return defaultServiceSpec()
}

func serviceDirectory(kind string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if kind == "launchd" {
		return filepath.Join(home, "Library", "LaunchAgents"), nil
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}
