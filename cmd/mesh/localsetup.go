package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"charm.land/huh/v2"

	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/cli"
)

// localStep is one command run on this machine, described the way the remote
// provisioning prompt describes its own.
type localStep struct {
	description string
	name        string
	args        []string
}

func (s localStep) display() string {
	return strings.TrimSpace(s.name + " " + strings.Join(s.args, " "))
}

// localTailscaleSetup installs and starts Tailscale here. Mesh already does
// this for the host being adopted, and refusing to do it for the machine you
// are sitting at is an arbitrary place to stop.
//
// Locally it is also the easier half: sudo and the browser login can have the
// terminal, so there is no password to relay and no auth key to create.
func localTailscaleSetup(ui bootstrapUI) bootstrap.LocalSetupFunc {
	return func(ctx context.Context, cause error) error {
		steps, err := localTailscaleSteps(runtime.GOOS, exec.LookPath)
		if err != nil {
			return err
		}
		ui.pauseSteps()
		if ui.terminal == nil || !ui.terminal() {
			return fmt.Errorf("%w; install Tailscale here, or run mesh add from a terminal to install it", cause)
		}

		if _, err := fmt.Fprintf(ui.output, "\nThis machine has no Tailscale, and adoption verifies the connection from here.\n\n"); err != nil {
			return err
		}
		for i, step := range steps {
			if _, err := fmt.Fprintf(ui.output, "  %d. %s\n     %s\n", i+1, step.description, step.display()); err != nil {
				return err
			}
		}
		var approved bool
		form := huh.NewForm(huh.NewGroup(
			huh.NewConfirm().
				Title("Set up Tailscale on this machine?").
				Affirmative("Install").
				Negative("Cancel").
				Value(&approved),
		)).WithInput(ui.input).WithOutput(ui.output)
		if err := form.RunWithContext(ctx); err != nil {
			return err
		}
		if !approved {
			return errors.New("Tailscale setup on this machine was declined")
		}

		for _, step := range steps {
			if _, err := fmt.Fprintf(ui.output, "\n%s %s\n", cli.Tag(ui.output, "LOCAL"), step.description); err != nil {
				return err
			}
			command := exec.CommandContext(ctx, step.name, step.args...)
			// sudo needs the real terminal for its password, and tailscale up
			// prints a login URL and waits. Both want stdio, not a buffer.
			command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := command.Run(); err != nil {
				return fmt.Errorf("%s: %w", step.display(), err)
			}
		}
		return nil
	}
}

// localTailscaleSteps plans the setup for this platform, skipping an install
// when the binary is already present and only the login is missing.
func localTailscaleSteps(goos string, lookPath func(string) (string, error)) ([]localStep, error) {
	installed := false
	if _, err := lookPath("tailscale"); err == nil {
		installed = true
	}

	var steps []localStep
	if !installed {
		switch goos {
		case "darwin":
			brew, err := lookPath("brew")
			if err != nil {
				return nil, errors.New("Homebrew is required to install Tailscale here; install it from https://brew.sh, or install Tailscale from https://tailscale.com/download")
			}
			steps = append(steps,
				localStep{"install Tailscale with Homebrew", brew, []string{"install", "tailscale"}},
				localStep{"start the Tailscale daemon now and at boot", "sudo", []string{brew, "services", "start", "tailscale"}},
			)
		case "linux":
			manager, err := localLinuxPackageManager(lookPath)
			if err != nil {
				return nil, err
			}
			steps = append(steps, manager...)
		default:
			return nil, fmt.Errorf("installing Tailscale on %s is not supported here; install it from https://tailscale.com/download", goos)
		}
	}
	steps = append(steps, localStep{"log in to your tailnet in the browser", "tailscale", []string{"up"}})
	return steps, nil
}

func localLinuxPackageManager(lookPath func(string) (string, error)) ([]localStep, error) {
	if pacman, err := lookPath("pacman"); err == nil {
		return []localStep{
			{"install Tailscale with pacman", "sudo", []string{pacman, "-S", "--needed", "--noconfirm", "tailscale"}},
			{"start the Tailscale daemon now and at boot", "sudo", []string{"systemctl", "enable", "--now", "tailscaled"}},
		}, nil
	}
	if _, err := lookPath("apt-get"); err == nil {
		return nil, errors.New("install Tailscale with the official repository from https://tailscale.com/download/linux, then run mesh add again")
	}
	return nil, errors.New("no supported package manager here; install Tailscale from https://tailscale.com/download")
}
