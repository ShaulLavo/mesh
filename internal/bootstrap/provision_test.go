package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestProvisionMissingPacmanRequiresConsentBeforeMutation(t *testing.T) {
	t.Parallel()

	var commands []string
	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		commands = append(commands, command)
		switch command {
		case linuxUserServiceProbeCommand:
			return []byte("MESH_USER=alice\nMESH_UID=1000\nLinger=no\n"), nil, nil
		case linuxInstallMethodCommand:
			return []byte("MESH_INSTALL_METHOD=pacman\n"), nil, nil
		case sudoCapabilityProbeCommand:
			return noninteractiveSudoProbe(), nil, nil
		default:
			t.Fatalf("mutation ran before consent: %q", command)
			return nil, nil, nil
		}
	}}
	confirmations := 0
	_, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform:    Platform{OS: Linux, Arch: ARM64},
		Target:      "alice@pi",
		Observation: tailscaleObservation{State: tailscaleMissing},
		Confirm: func(_ context.Context, confirmation ProvisionConfirmation) (bool, error) {
			confirmations++
			if confirmation.PackageManager != "pacman" || !strings.Contains(confirmation.Summary, "alice@pi has no Tailscale") {
				t.Fatalf("confirmation = %#v", confirmation)
			}
			joined := strings.Join(confirmation.Commands, "\n")
			if !strings.Contains(joined, noninteractiveSudoCommand("pacman -S --needed --noconfirm tailscale")) || !strings.Contains(joined, "loginctl enable-linger 'alice'") {
				t.Fatalf("confirmation commands = %q", joined)
			}
			return false, nil
		},
	})
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
	if confirmations != 1 || !reflect.DeepEqual(commands, []string{linuxUserServiceProbeCommand, linuxInstallMethodCommand, sudoCapabilityProbeCommand}) {
		t.Fatalf("confirmations = %d, commands = %#v", confirmations, commands)
	}
}

func TestProvisionPacmanAuthenticatesOnlyThroughStdinAndEnablesLingering(t *testing.T) {
	t.Parallel()

	const secret = "tskey-auth-super-secret-fixture" //nolint:gosec // inert sentinel proves the auth key is never recorded
	statusCalls := 0
	var commands []string
	var authInput string
	remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
		commands = append(commands, command)
		switch command {
		case linuxUserServiceProbeCommand:
			return []byte("MESH_USER=alice\nMESH_UID=1000\nLinger=no\n"), nil, nil
		case linuxInstallMethodCommand:
			return []byte("MESH_INSTALL_METHOD=pacman\n"), nil, nil
		case sudoCapabilityProbeCommand:
			return noninteractiveSudoProbe(), nil, nil
		case noninteractiveSudoCommand("pacman -S --needed --noconfirm tailscale"), noninteractiveSudoCommand("systemctl enable --now tailscaled"), noninteractiveSudoCommand("loginctl enable-linger 'alice'"):
			return nil, nil, nil
		case "loginctl show-user 'alice' --property=Linger":
			return []byte("Linger=yes\n"), nil, nil
		case tailscaleStatusCommand:
			statusCalls++
			if statusCalls == 1 {
				return []byte(`{"BackendState":"NeedsLogin"}`), nil, nil
			}
			return runningStatus("pi.tail.example", "100.64.0.8"), nil, nil
		case noninteractiveSudoCommand("tailscale up --auth-key=file:/dev/stdin"):
			contents, err := io.ReadAll(stdin)
			if err != nil {
				t.Fatal(err)
			}
			authInput = string(contents)
			return nil, nil, nil
		default:
			t.Fatalf("unexpected command %q", command)
			return nil, nil, nil
		}
	}}
	var progress []Event
	var confirmation ProvisionConfirmation
	result, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform:    Platform{OS: Linux, Arch: ARM64},
		Target:      "alice@pi",
		Observation: tailscaleObservation{State: tailscaleMissing},
		AuthKey:     []byte(secret),
		Confirm: func(_ context.Context, got ProvisionConfirmation) (bool, error) {
			confirmation = got
			return true, nil
		},
		Progress: func(event Event) { progress = append(progress, event) },
	})
	if err != nil {
		t.Fatalf("provisionRemote() error = %v", err)
	}
	if !result.Changed || result.Tailnet.Name != "pi.tail.example" || authInput != secret+"\n" {
		t.Fatalf("result = %#v, auth stdin = %q", result, authInput)
	}
	recorded := strings.Join(commands, "\n") + strings.Join(confirmation.Commands, "\n") + formatEvents(progress)
	if strings.Contains(recorded, secret) {
		t.Fatalf("secret leaked into recorded output: %q", recorded)
	}
	if !strings.Contains(recorded, "--auth-key=file:/dev/stdin") {
		t.Fatalf("recorded commands do not name stdin auth: %q", recorded)
	}
}

func TestProvisionUsesSudoPasswordWithoutPuttingSecretsInArgv(t *testing.T) {
	t.Parallel()

	const (
		sudoPassword = "remote-sudo-password"
		authKey      = "tskey-auth-sudo-fixture"
	)
	statusCalls := 0
	passwordPrompts := 0
	var confirmed []string
	var installCommands []string
	remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
		if strings.Contains(command, sudoPassword) || strings.Contains(command, authKey) {
			t.Fatalf("secret appeared in argv: %q", command)
		}
		switch {
		case command == linuxUserServiceProbeCommand:
			return []byte("MESH_USER=alice\nMESH_UID=1000\nLinger=yes\n"), nil, nil
		case command == linuxInstallMethodCommand:
			return []byte("MESH_INSTALL_METHOD=pacman\n"), nil, nil
		case strings.Contains(command, "command -v sudo"):
			return []byte("MESH_SUDO_MODE=password\nMESH_SUDO_PATH=/usr/bin/sudo\n"), nil, nil
		case strings.HasSuffix(command, " -n true"):
			if !strings.Contains(command, "-S -p '' -v") {
				t.Fatalf("sudo validation command = %q", command)
			}
			assertReaderContents(t, stdin, sudoPassword+"\n")
			return nil, nil, nil
		case strings.Contains(command, "pacman -S --needed --noconfirm tailscale"), strings.Contains(command, "systemctl enable --now tailscaled"):
			if !strings.Contains(command, "-S -p '' -v") || !strings.Contains(command, "'/usr/bin/sudo' -n ") {
				t.Fatalf("password sudo command = %q", command)
			}
			assertReaderContents(t, stdin, sudoPassword+"\n")
			installCommands = append(installCommands, command)
			return nil, nil, nil
		case command == tailscaleStatusCommand:
			statusCalls++
			if statusCalls == 1 {
				return []byte(`{"BackendState":"NeedsLogin"}`), nil, nil
			}
			return runningStatus("pi.tail.example", "100.64.0.8"), nil, nil
		case strings.Contains(command, "tailscale up --auth-key=file:/dev/stdin"):
			if !strings.Contains(command, "-S -p '' -v") || !strings.Contains(command, "'/usr/bin/sudo' -n ") {
				t.Fatalf("password sudo auth command = %q", command)
			}
			assertReaderContents(t, stdin, sudoPassword+"\n"+authKey+"\n")
			return nil, nil, nil
		default:
			t.Fatalf("unexpected command %q", command)
			return nil, nil, nil
		}
	}}
	result, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Linux, Arch: ARM64}, Target: "alice@pi",
		Observation: tailscaleObservation{State: tailscaleMissing}, AuthKey: []byte(authKey),
		Confirm: func(_ context.Context, confirmation ProvisionConfirmation) (bool, error) {
			confirmed = append([]string(nil), confirmation.Commands...)
			if len(confirmation.Checks) != 1 || !strings.Contains(confirmation.Checks[0], "sudo password") {
				t.Fatalf("confirmation checks = %#v", confirmation.Checks)
			}
			return true, nil
		},
		SudoPassword: func(context.Context, string) ([]byte, error) {
			passwordPrompts++
			return []byte(sudoPassword), nil
		},
	})
	if err != nil || result.Tailnet.Name != "pi.tail.example" || passwordPrompts != 1 {
		t.Fatalf("provisionRemote() = %#v, %v after %d password prompts", result, err, passwordPrompts)
	}
	if !reflect.DeepEqual(confirmed, installCommands) || strings.Contains(strings.Join(confirmed, "\n"), sudoPassword) {
		t.Fatalf("confirmed commands = %#v, install commands = %#v", confirmed, installCommands)
	}
}

func TestProvisionConfirmationMatchesCommandsAfterInstall(t *testing.T) {
	t.Parallel()

	statusCalls := 0
	var commands []string
	var confirmed []string
	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		switch command {
		case linuxUserServiceProbeCommand:
			return []byte("MESH_USER=alice\nMESH_UID=1000\nLinger=yes\n"), nil, nil
		case linuxInstallMethodCommand:
			return []byte("MESH_INSTALL_METHOD=pacman\n"), nil, nil
		case sudoCapabilityProbeCommand:
			return noninteractiveSudoProbe(), nil, nil
		case tailscaleStatusCommand:
			statusCalls++
			if statusCalls == 1 {
				return []byte(`{"BackendState":"Stopped"}`), nil, nil
			}
			return runningStatus("pi.tail.example", "100.64.0.8"), nil, nil
		default:
			commands = append(commands, command)
			return nil, nil, nil
		}
	}}
	_, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Linux, Arch: ARM64}, Target: "alice@pi",
		Observation: tailscaleObservation{State: tailscaleMissing}, AuthKey: []byte("fixture-auth-key"),
		Confirm: func(_ context.Context, confirmation ProvisionConfirmation) (bool, error) {
			confirmed = append([]string(nil), confirmation.Commands...)
			return true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) < len(confirmed) || !reflect.DeepEqual(confirmed, commands[:len(confirmed)]) {
		t.Fatalf("confirmed commands = %#v, executed commands = %#v", confirmed, commands)
	}
}

func TestProvisionLoggedOutStatesRequireAuthKeyBeforeMutation(t *testing.T) {
	t.Parallel()

	for _, state := range []tailscaleState{tailscaleNeedsLogin, tailscaleNoState} {
		t.Run(state.String(), func(t *testing.T) {
			remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
				t.Fatalf("remote command ran without an auth key: %q", command)
				return nil, nil, nil
			}}
			_, err := provisionRemote(context.Background(), remote, provisionRequest{
				Platform: Platform{OS: Linux, Arch: AMD64}, Observation: tailscaleObservation{State: state},
			})
			assertDiagnosticCode(t, err, DiagnosticTailscaleLoggedOut)
			if !strings.Contains(err.Error(), "--tailscale-auth-key-file") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestProvisionLoggedOutStatesAuthenticateWithKey(t *testing.T) {
	t.Parallel()

	for _, state := range []tailscaleState{tailscaleNeedsLogin, tailscaleNoState} {
		t.Run(state.String(), func(t *testing.T) {
			upCalls := 0
			remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
				switch command {
				case linuxUserServiceProbeCommand:
					return []byte("MESH_USER=alice\nMESH_UID=1000\nLinger=yes\n"), nil, nil
				case sudoCapabilityProbeCommand:
					return noninteractiveSudoProbe(), nil, nil
				case noninteractiveSudoCommand("tailscale up --auth-key=file:/dev/stdin"):
					upCalls++
					if contents, err := io.ReadAll(stdin); err != nil || string(contents) != "fixture-key\n" {
						t.Fatalf("auth stdin = %q, %v", contents, err)
					}
					return nil, nil, nil
				case tailscaleStatusCommand:
					return runningStatus("pi.tail.example", "100.64.0.8"), nil, nil
				default:
					t.Fatalf("unexpected command %q", command)
					return nil, nil, nil
				}
			}}
			result, err := provisionRemote(context.Background(), remote, provisionRequest{
				Platform: Platform{OS: Linux, Arch: AMD64}, Observation: tailscaleObservation{State: state}, AuthKey: []byte("fixture-key"),
			})
			if err != nil || !result.Changed || upCalls != 1 {
				t.Fatalf("provisionRemote() = %#v, %v, up calls %d", result, err, upCalls)
			}
		})
	}
}

func TestProvisionReportsInstalledTailscaleWhenAuthenticationCannotContinue(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		switch command {
		case linuxUserServiceProbeCommand:
			return []byte("MESH_USER=alice\nMESH_UID=1000\nLinger=yes\n"), nil, nil
		case linuxInstallMethodCommand:
			return []byte("MESH_INSTALL_METHOD=pacman\n"), nil, nil
		case sudoCapabilityProbeCommand:
			return noninteractiveSudoProbe(), nil, nil
		case noninteractiveSudoCommand("pacman -S --needed --noconfirm tailscale"), noninteractiveSudoCommand("systemctl enable --now tailscaled"):
			return nil, nil, nil
		case tailscaleStatusCommand:
			return []byte(`{"BackendState":"NeedsLogin"}`), nil, nil
		default:
			t.Fatalf("unexpected command %q", command)
			return nil, nil, nil
		}
	}}
	_, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Linux, Arch: AMD64}, Target: "alice@pi",
		Observation: tailscaleObservation{State: tailscaleMissing},
		Confirm:     func(context.Context, ProvisionConfirmation) (bool, error) { return true, nil },
	})
	assertDiagnosticCode(t, err, DiagnosticTailscaleLoggedOut)
	for _, want := range []string{"Tailscale was installed", "Mesh was not installed", "--tailscale-auth-key-file"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
}

func TestProvisionReportsInstalledTailscaleWhenPostInstallConvergenceFails(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		switch command {
		case linuxUserServiceProbeCommand:
			return []byte("MESH_USER=alice\nMESH_UID=1000\nLinger=yes\n"), nil, nil
		case linuxInstallMethodCommand:
			return []byte("MESH_INSTALL_METHOD=pacman\n"), nil, nil
		case sudoCapabilityProbeCommand:
			return noninteractiveSudoProbe(), nil, nil
		case noninteractiveSudoCommand("pacman -S --needed --noconfirm tailscale"), noninteractiveSudoCommand("systemctl enable --now tailscaled"):
			return nil, nil, nil
		case tailscaleStatusCommand:
			return []byte(`{"BackendState":"Stopped"}`), nil, nil
		case noninteractiveSudoCommand("tailscale up"):
			return nil, []byte("backend refused to start"), errors.New("exit 1")
		default:
			t.Fatalf("unexpected command %q", command)
			return nil, nil, nil
		}
	}}
	_, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Linux, Arch: AMD64}, Target: "alice@pi", Observation: tailscaleObservation{State: tailscaleMissing},
		Confirm: func(context.Context, ProvisionConfirmation) (bool, error) { return true, nil },
	})
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
	for _, want := range []string{"Tailscale was installed", "Mesh was not installed", "backend refused to start"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
}

func TestProvisionStoppedRunsUpWithoutAuthKey(t *testing.T) {
	t.Parallel()

	upCalls := 0
	remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
		switch command {
		case linuxUserServiceProbeCommand:
			return []byte("MESH_USER=alice\nMESH_UID=1000\nLinger=yes\n"), nil, nil
		case sudoCapabilityProbeCommand:
			return noninteractiveSudoProbe(), nil, nil
		case noninteractiveSudoCommand("tailscale up"):
			upCalls++
			if stdin != nil {
				t.Fatal("tailscale up received stdin for Stopped")
			}
			return nil, nil, nil
		case tailscaleStatusCommand:
			return runningStatus("pi.tail.example", "100.64.0.8"), nil, nil
		default:
			t.Fatalf("unexpected command %q", command)
			return nil, nil, nil
		}
	}}
	result, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Linux, Arch: AMD64}, Observation: tailscaleObservation{State: tailscaleStopped},
		Confirm: func(context.Context, ProvisionConfirmation) (bool, error) {
			t.Fatal("Stopped with lingering enabled asked for confirmation")
			return false, nil
		},
	})
	if err != nil || !result.Changed || upCalls != 1 {
		t.Fatalf("provisionRemote() = %#v, %v, up calls %d", result, err, upCalls)
	}
}

func TestProvisionRunningIsNoOp(t *testing.T) {
	t.Parallel()

	observation := tailscaleObservation{State: tailscaleRunning, Tailnet: tailnetObservation{Name: "pi.tail.example", Addresses: []string{"100.64.0.8"}}}
	commands := 0
	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		commands++
		if command != linuxUserServiceProbeCommand {
			t.Fatalf("already-running Tailscale received command %q", command)
		}
		return []byte("MESH_USER=alice\nMESH_UID=1000\nLinger=yes\n"), nil, nil
	}}
	result, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Linux, Arch: AMD64}, Observation: observation,
		Confirm: func(context.Context, ProvisionConfirmation) (bool, error) {
			t.Fatal("no-op provision asked for confirmation")
			return false, nil
		},
	})
	if err != nil || result.Changed || commands != 1 || !reflect.DeepEqual(result.Tailnet, observation.Tailnet) {
		t.Fatalf("provisionRemote() = %#v, %v after %d commands", result, err, commands)
	}
}

func TestProvisionNeedsMachineAuthNeverRetries(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		t.Fatalf("NeedsMachineAuth ran remote command %q", command)
		return nil, nil, nil
	}}
	_, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Linux, Arch: AMD64}, Observation: tailscaleObservation{State: tailscaleNeedsMachineAuth},
	})
	assertDiagnosticCode(t, err, DiagnosticTailscaleMachineAuth)
	if !strings.Contains(err.Error(), "admin console") {
		t.Fatalf("error = %v", err)
	}
}

func TestProvisionDarwinUsesExistingApplicationCLI(t *testing.T) {
	t.Parallel()

	const secret = "tskey-auth-darwin-fixture" //nolint:gosec // inert sentinel proves the auth key stays on stdin
	var commands []string
	remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
		commands = append(commands, command)
		switch command {
		case tailscaleUpCommand(Platform{OS: Darwin, Arch: ARM64}, tailscaleObservation{Variant: tailscaleVariantApplication, CLIPath: tailscaleApplicationCLI}, privilegeSpec{Mode: privilegeRoot}, true):
			contents, err := io.ReadAll(stdin)
			if err != nil || string(contents) != secret+"\n" {
				t.Fatalf("auth stdin = %q, %v", contents, err)
			}
			return nil, nil, nil
		case tailscaleStatusCommand:
			return runningStatus("mac.tail.example", "100.64.0.9"), nil, nil
		default:
			t.Fatalf("unexpected command %q", command)
			return nil, nil, nil
		}
	}}
	result, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Darwin, Arch: ARM64}, Observation: tailscaleObservation{State: tailscaleNeedsLogin, Variant: tailscaleVariantApplication, CLIPath: tailscaleApplicationCLI}, AuthKey: []byte(secret),
	})
	if err != nil || result.Tailnet.Name != "mac.tail.example" {
		t.Fatalf("provisionRemote() = %#v, %v", result, err)
	}
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, tailscaleApplicationCLI) || strings.Contains(joined, "brew install") || strings.Contains(joined, "brew services") || strings.Contains(joined, secret) {
		t.Fatalf("Darwin application commands = %q", joined)
	}
}

func TestDetectBrewPathChecksStandardAppleSiliconLocation(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		if !strings.Contains(command, "/opt/homebrew/bin/brew") {
			t.Fatalf("Homebrew probe = %q", command)
		}
		return []byte("MESH_BREW_PATH=/opt/homebrew/bin/brew\n"), nil, nil
	}}
	got, err := detectBrewPath(context.Background(), remote)
	if err != nil || got != "/opt/homebrew/bin/brew" {
		t.Fatalf("detectBrewPath() = %q, %v", got, err)
	}
}

func TestProvisionDarwinInstallsWithHomebrew(t *testing.T) {
	t.Parallel()

	statusCalls := 0
	var commands []string
	remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
		commands = append(commands, command)
		switch command {
		case "id -u":
			return []byte("501\n"), nil, nil
		case "if [ -d '/Applications/Tailscale.app' ]; then printf 'yes\\n'; else printf 'no\\n'; fi":
			return []byte("no\n"), nil, nil
		case brewPathProbeCommand:
			return []byte("MESH_BREW_PATH=/opt/homebrew/bin/brew\n"), nil, nil
		case sudoCapabilityProbeCommand:
			return noninteractiveSudoProbe(), nil, nil
		case "'/opt/homebrew/bin/brew' install tailscale", noninteractiveSudoCommand("'/opt/homebrew/bin/brew' services start tailscale"):
			return nil, nil, nil
		case tailscaleStatusCommand:
			statusCalls++
			if statusCalls == 1 {
				return []byte(`{"BackendState":"NeedsLogin"}`), nil, nil
			}
			return runningStatus("mac.tail.example", "100.64.0.9"), nil, nil
		case tailscaleUpCommand(Platform{OS: Darwin, Arch: ARM64}, tailscaleObservation{}, noninteractiveSudoSpec(), true):
			if contents, err := io.ReadAll(stdin); err != nil || string(contents) != "tskey-auth-mac\n" {
				t.Fatalf("auth stdin = %q, %v", contents, err)
			}
			return nil, nil, nil
		default:
			t.Fatalf("unexpected command %q", command)
			return nil, nil, nil
		}
	}}
	result, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Darwin, Arch: ARM64}, Target: "alice@mac",
		Observation: tailscaleObservation{State: tailscaleMissing}, AuthKey: []byte("tskey-auth-mac"),
		Confirm: func(_ context.Context, confirmation ProvisionConfirmation) (bool, error) {
			if confirmation.PackageManager != "Homebrew" || !strings.Contains(strings.Join(confirmation.Commands, "\n"), "'/opt/homebrew/bin/brew' install tailscale") {
				t.Fatalf("confirmation = %#v", confirmation)
			}
			return true, nil
		},
	})
	if err != nil || !result.Changed || result.Tailnet.Name != "mac.tail.example" {
		t.Fatalf("provisionRemote() = %#v, %v", result, err)
	}
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, "brew' install tailscale") || !strings.Contains(joined, noninteractiveSudoCommand("'/opt/homebrew/bin/brew' services start tailscale")) {
		t.Fatalf("Homebrew commands = %q", joined)
	}
}

func TestProvisionReportsPartialHomebrewInstall(t *testing.T) {
	t.Parallel()

	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		switch command {
		case "id -u":
			return []byte("501\n"), nil, nil
		case "if [ -d '/Applications/Tailscale.app' ]; then printf 'yes\\n'; else printf 'no\\n'; fi":
			return []byte("no\n"), nil, nil
		case brewPathProbeCommand:
			return []byte("MESH_BREW_PATH=/opt/homebrew/bin/brew\n"), nil, nil
		case sudoCapabilityProbeCommand:
			return noninteractiveSudoProbe(), nil, nil
		case "'/opt/homebrew/bin/brew' install tailscale":
			return nil, nil, nil
		case noninteractiveSudoCommand("'/opt/homebrew/bin/brew' services start tailscale"):
			return nil, []byte("sudo: a password is required"), errors.New("exit 1")
		default:
			t.Fatalf("unexpected command %q", command)
			return nil, nil, nil
		}
	}}
	_, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Darwin, Arch: ARM64}, Target: "alice@mac",
		Observation: tailscaleObservation{State: tailscaleMissing},
		Confirm:     func(context.Context, ProvisionConfirmation) (bool, error) { return true, nil },
	})
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
	for _, want := range []string{"Tailscale was installed", "Mesh was not installed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
}

func TestProvisionDarwinValidatesSudoPasswordBeforeHomebrewMutation(t *testing.T) {
	t.Parallel()

	const secret = "mac-sudo-password-fixture" //nolint:gosec // inert sentinel verifies preflight and redaction
	brewMutations := 0
	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		switch command {
		case "if [ -d '/Applications/Tailscale.app' ]; then printf 'yes\\n'; else printf 'no\\n'; fi":
			return []byte("no\n"), nil, nil
		case brewPathProbeCommand:
			return []byte("MESH_BREW_PATH=/opt/homebrew/bin/brew\n"), nil, nil
		case "id -u":
			return []byte("501\n"), nil, nil
		case sudoCapabilityProbeCommand:
			return []byte("MESH_SUDO_MODE=password\nMESH_SUDO_PATH=/usr/bin/sudo\n"), nil, nil
		default:
			if strings.Contains(command, "brew' install tailscale") || strings.Contains(command, "services start tailscale") {
				brewMutations++
				t.Fatalf("Homebrew mutation ran before sudo validation: %q", command)
			}
			return []byte(secret), []byte(secret), errors.New(secret)
		}
	}}
	_, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Darwin, Arch: ARM64}, Target: "alice@mac", Observation: tailscaleObservation{State: tailscaleMissing},
		Confirm: func(context.Context, ProvisionConfirmation) (bool, error) { return true, nil },
		SudoPassword: func(context.Context, string) ([]byte, error) {
			return []byte(secret), nil
		},
	})
	assertDiagnosticCode(t, err, DiagnosticSudoAuth)
	if brewMutations != 0 || strings.Contains(err.Error(), secret) {
		t.Fatalf("Homebrew mutations = %d, error = %v", brewMutations, err)
	}
}

func TestPinnedInstallerChecksumMismatchNeverExecutes(t *testing.T) {
	t.Parallel()

	commands := 0
	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		commands++
		if commands != 1 || !strings.Contains(command, pinnedTailscaleInstallerURL) {
			t.Fatalf("installer executed after checksum mismatch: %q", command)
		}
		return []byte("tampered installer"), nil, nil
	}}
	_, err := applyInstallPlan(context.Background(), remote, installPlan{
		Kind: installPinnedScript, Downloader: "curl", InstallerURL: pinnedTailscaleInstallerURL, InstallerSHA256: pinnedTailscaleInstallerSHA256,
	}, privilege{Spec: noninteractiveSudoSpec()})
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
	if commands != 1 || !strings.Contains(err.Error(), "was not executed") {
		t.Fatalf("commands = %d, error = %v", commands, err)
	}
}

func TestApplyAPTInstallPlanSucceedsWithBoundedDownloads(t *testing.T) {
	t.Parallel()

	plan := installPlan{Kind: installAPT, RepoOS: "ubuntu", Codename: "noble", Downloader: "curl"}
	wantCommands := plan.renderedCommands(privilegeSpec{Mode: privilegeRoot})
	key := []byte("fixture repository key")
	list := []byte("deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/ubuntu noble main\n")
	var commands []string
	remote := &stubRemote{runWithLimits: func(command string, stdin io.Reader, limits []remoteOutputLimits) ([]byte, []byte, error) {
		index := len(commands)
		commands = append(commands, command)
		if index >= len(wantCommands) || command != wantCommands[index] {
			t.Fatalf("command %d = %q, want %q", index, command, wantCommands[index])
		}
		switch index {
		case 0:
			assertOutputLimits(t, limits, maximumRepositoryAssetBytes+1)
			return key, nil, nil
		case 1:
			assertOutputLimits(t, limits, maximumRepositoryAssetBytes+1)
			return list, nil, nil
		case 3:
			assertReaderContents(t, stdin, string(key))
		case 4:
			assertReaderContents(t, stdin, string(list))
		default:
			if stdin != nil {
				t.Fatalf("command %d received unexpected stdin", index)
			}
		}
		return nil, nil, nil
	}}
	outcome, err := applyInstallPlan(context.Background(), remote, plan, privilege{Spec: privilegeSpec{Mode: privilegeRoot}})
	if err != nil || !outcome.Changed || !outcome.PackageInstalled || !reflect.DeepEqual(commands, wantCommands) {
		t.Fatalf("applyInstallPlan() = %#v, %v; commands = %#v", outcome, err, commands)
	}
}

func TestApplyPinnedInstallerSucceedsAfterChecksumWithBoundedDownload(t *testing.T) {
	t.Parallel()

	installer := []byte("#!/bin/sh\nprintf 'fixture installer ran\\n'\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(installer))
	plan := installPlan{
		Kind: installPinnedScript, Downloader: "wget", InstallerURL: "https://example.invalid/installer.sh", InstallerSHA256: digest,
	}
	commands := 0
	remote := &stubRemote{runWithLimits: func(command string, stdin io.Reader, limits []remoteOutputLimits) ([]byte, []byte, error) {
		commands++
		switch commands {
		case 1:
			if command != "wget -q --tries=4 --waitretry=2 --timeout=30 -O- 'https://example.invalid/installer.sh'" || stdin != nil {
				t.Fatalf("download command = %q, stdin = %v", command, stdin)
			}
			assertOutputLimits(t, limits, maximumInstallerBytes+1)
			return installer, nil, nil
		case 2:
			if command != "/bin/sh -s" || len(limits) != 0 {
				t.Fatalf("execute command = %q, limits = %#v", command, limits)
			}
			assertReaderContents(t, stdin, string(installer))
			return nil, nil, nil
		default:
			t.Fatalf("unexpected command %q", command)
			return nil, nil, nil
		}
	}}
	outcome, err := applyInstallPlan(context.Background(), remote, plan, privilege{Spec: privilegeSpec{Mode: privilegeRoot}})
	if err != nil || !outcome.Changed || !outcome.PackageInstalled || commands != 2 {
		t.Fatalf("applyInstallPlan() = %#v, %v after %d commands", outcome, err, commands)
	}
	if checks := plan.checks(); len(checks) != 1 || !strings.Contains(checks[0], digest) {
		t.Fatalf("installer checks = %#v", checks)
	}
}

func TestApplyPinnedInstallerRejectsOversizedDownloadBeforeMutation(t *testing.T) {
	t.Parallel()

	commands := 0
	remote := &stubRemote{runWithLimits: func(_ string, _ io.Reader, limits []remoteOutputLimits) ([]byte, []byte, error) {
		commands++
		assertOutputLimits(t, limits, maximumInstallerBytes+1)
		return bytes.Repeat([]byte("x"), maximumInstallerBytes+1), nil, nil
	}}
	_, err := applyInstallPlan(context.Background(), remote, installPlan{
		Kind: installPinnedScript, Downloader: "curl", InstallerURL: "https://example.invalid/installer.sh", InstallerSHA256: strings.Repeat("0", 64),
	}, privilege{Spec: privilegeSpec{Mode: privilegeRoot}})
	assertDiagnosticCode(t, err, DiagnosticTailscaleUnavailable)
	if commands != 1 || !strings.Contains(err.Error(), "invalid size") {
		t.Fatalf("commands = %d, error = %v", commands, err)
	}
}

func TestPrivilegeClearsAndRedactsPasswordReturnedWithPromptError(t *testing.T) {
	t.Parallel()

	const secret = "sudo-prompt-error-secret" //nolint:gosec // inert sentinel verifies cleanup and redaction
	provided := []byte(secret)
	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		t.Fatalf("remote command ran after prompt error: %q", command)
		return nil, nil, nil
	}}
	_, err := (privilegeSpec{Mode: privilegeSudoPassword, SudoPath: "/usr/bin/sudo"}).acquire(context.Background(), remote, "alice@pi", func(context.Context, string) ([]byte, error) {
		return provided, errors.New("prompt failed with " + secret)
	})
	assertDiagnosticCode(t, err, DiagnosticSudoAuth)
	if strings.Contains(err.Error(), secret) || !bytes.Equal(provided, make([]byte, len(provided))) {
		t.Fatalf("password was retained: bytes = %q, error = %v", provided, err)
	}
}

func TestProvisionRejectsBadSudoPasswordBeforeMutation(t *testing.T) {
	t.Parallel()

	const secret = "bad-sudo-password"
	mutations := 0
	remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
		switch command {
		case linuxUserServiceProbeCommand:
			return []byte("MESH_USER=alice\nMESH_UID=1000\nLinger=yes\n"), nil, nil
		case linuxInstallMethodCommand:
			return []byte("MESH_INSTALL_METHOD=pacman\n"), nil, nil
		case sudoCapabilityProbeCommand:
			return []byte("MESH_SUDO_MODE=password\nMESH_SUDO_PATH=/usr/bin/sudo\n"), nil, nil
		default:
			if strings.Contains(command, "pacman -S") || strings.Contains(command, "systemctl enable") {
				mutations++
				t.Fatalf("mutation ran after rejected sudo password: %q", command)
			}
			return []byte(secret), []byte(secret), errors.New(secret)
		}
	}}
	_, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Linux, Arch: ARM64}, Target: "alice@pi", Observation: tailscaleObservation{State: tailscaleMissing},
		Confirm: func(context.Context, ProvisionConfirmation) (bool, error) { return true, nil },
		SudoPassword: func(context.Context, string) ([]byte, error) {
			return []byte(secret), nil
		},
	})
	assertDiagnosticCode(t, err, DiagnosticSudoAuth)
	if mutations != 0 || strings.Contains(err.Error(), secret) {
		t.Fatalf("mutations = %d, error = %v", mutations, err)
	}
}

func TestAuthFailureDoesNotEchoRemoteSecret(t *testing.T) {
	t.Parallel()

	const secret = "tskey-auth-must-not-escape" //nolint:gosec // inert sentinel must not survive in an error
	remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
		if strings.Contains(command, secret) {
			t.Fatalf("secret appeared in argv: %q", command)
		}
		if contents, err := io.ReadAll(stdin); err != nil || string(contents) != secret+"\n" {
			t.Fatalf("auth stdin = %q, %v", contents, err)
		}
		return []byte(secret), []byte(secret), errors.New(secret)
	}}
	err := runTailscaleUp(context.Background(), remote, Platform{OS: Linux, Arch: AMD64}, tailscaleObservation{}, privilege{Spec: noninteractiveSudoSpec()}, []byte(secret))
	assertDiagnosticCode(t, err, DiagnosticTailscaleLoggedOut)
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("secret leaked in error: %v", err)
	}
}

func TestPostAuthStatusFailureDoesNotEchoRemoteSecret(t *testing.T) {
	t.Parallel()

	const secret = "tskey-auth-post-status-fixture" //nolint:gosec // inert sentinel must not survive status verification
	remote := &stubRemote{run: func(command string, stdin io.Reader) ([]byte, []byte, error) {
		switch command {
		case linuxUserServiceProbeCommand:
			return []byte("MESH_USER=alice\nMESH_UID=1000\nLinger=yes\n"), nil, nil
		case sudoCapabilityProbeCommand:
			return noninteractiveSudoProbe(), nil, nil
		case noninteractiveSudoCommand("tailscale up --auth-key=file:/dev/stdin"):
			if contents, err := io.ReadAll(stdin); err != nil || string(contents) != secret+"\n" {
				t.Fatalf("auth stdin = %q, %v", contents, err)
			}
			return nil, nil, nil
		case tailscaleStatusCommand:
			return []byte(secret), []byte(secret), errors.New(secret)
		default:
			t.Fatalf("unexpected command %q", command)
			return nil, nil, nil
		}
	}}
	_, err := provisionRemote(context.Background(), remote, provisionRequest{
		Platform: Platform{OS: Linux, Arch: AMD64}, Observation: tailscaleObservation{State: tailscaleNeedsLogin}, AuthKey: []byte(secret),
	})
	assertDiagnosticCode(t, err, DiagnosticTailscaleLoggedOut)
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("secret leaked in post-auth status error: %v", err)
	}
}

func TestAuthCommandReadsKeyFromDevStdin(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	fixture := directory + "/tailscale"
	script := "#!/bin/sh\n[ \"$1\" = up ]\n[ \"$2\" = --auth-key=file:/dev/stdin ]\n/bin/cat /dev/stdin\n"
	if err := os.WriteFile(fixture, []byte(script), 0o700); err != nil { //nolint:gosec // the test fixture must be executable
		t.Fatal(err)
	}
	const secret = "stdin-auth-key-fixture"                                                                                                                                                       //nolint:gosec // inert sentinel proves /dev/stdin reaches the CLI
	command := exec.CommandContext(context.Background(), "/bin/sh", "-c", tailscaleUpCommand(Platform{OS: Linux, Arch: AMD64}, tailscaleObservation{}, privilegeSpec{Mode: privilegeRoot}, true)) //nolint:gosec // the command is generated from fixed internal values
	command.Env = append(os.Environ(), "PATH="+directory)
	command.Stdin = strings.NewReader(secret)
	output, err := command.Output()
	if err != nil || string(output) != secret {
		t.Fatalf("auth command output = %q, error = %v", output, err)
	}
}

func TestPasswordSudoKeepsAuthKeyAsTailscaleOnlyStdin(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	parentPath := directory + "/sudo-parent"
	sudoPath := directory + "/sudo"
	sudoScript := fmt.Sprintf(`#!/bin/sh
case "$1" in
  -S)
    [ "$2" = -p ] && [ -z "$3" ] && [ "$4" = -v ] || exit 10
    IFS= read -r password
    [ "$password" = sudo-password-fixture ] || exit 11
    printf '%%s\n' "$PPID" > %s
    ;;
  -n)
    shift
    [ -r %s ] && [ "$(cat %s)" = "$PPID" ] || exit 12
    exec "$@"
    ;;
  *) exit 13 ;;
esac
`, shellQuote(parentPath), shellQuote(parentPath), shellQuote(parentPath))
	if err := os.WriteFile(sudoPath, []byte(sudoScript), 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	tailscalePath := directory + "/tailscale"
	tailscaleScript := "#!/bin/sh\n[ \"$1\" = up ]\n[ \"$2\" = --auth-key=file:/dev/stdin ]\n/bin/cat /dev/stdin\n"
	if err := os.WriteFile(tailscalePath, []byte(tailscaleScript), 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	const secret = "stdin-auth-key-behind-sudo" //nolint:gosec // inert sentinel verifies stdin separation
	commandText := tailscaleUpCommand(
		Platform{OS: Linux, Arch: AMD64},
		tailscaleObservation{Variant: tailscaleVariantSystem, CLIPath: tailscalePath},
		privilegeSpec{Mode: privilegeSudoPassword, SudoPath: sudoPath},
		true,
	)
	command := exec.CommandContext(context.Background(), "/bin/sh", "-c", commandText) //nolint:gosec // generated only from fixed test paths and internal commands
	command.Stdin = strings.NewReader("sudo-password-fixture\n" + secret)
	output, err := command.Output()
	if err != nil || string(output) != secret {
		t.Fatalf("password sudo auth output = %q, error = %v; command = %q", output, err, commandText)
	}
}

func TestDetectLinuxInstallPlanPrefersAPTRepository(t *testing.T) {
	t.Parallel()

	for _, downloader := range []string{"curl", "wget"} {
		t.Run(downloader, func(t *testing.T) {
			remote := &stubRemote{run: func(command string, _ io.Reader) ([]byte, []byte, error) {
				if command != linuxInstallMethodCommand {
					t.Fatalf("command = %q", command)
				}
				return []byte("MESH_INSTALL_METHOD=apt\nMESH_REPO_OS=ubuntu\nMESH_REPO_VERSION=noble\nMESH_DOWNLOADER=" + downloader + "\n"), nil, nil
			}}
			plan, err := detectLinuxInstallPlan(context.Background(), remote)
			if err != nil || plan.Kind != installAPT || plan.Downloader != downloader {
				t.Fatalf("detectLinuxInstallPlan() = %#v, %v", plan, err)
			}
			commands := strings.Join(plan.renderedCommands(noninteractiveSudoSpec()), "\n")
			if !strings.Contains(commands, "stable/ubuntu/noble.noarmor.gpg") || !strings.Contains(commands, "apt-get install -y tailscale") || strings.Contains(commands, "installer.sh") || !strings.Contains(commands, downloader) {
				t.Fatalf("apt commands = %q", commands)
			}
		})
	}
}

func TestLinuxInstallProbeClassifiesDistroBeforeExecutables(t *testing.T) {
	t.Parallel()

	release := strings.Index(linuxInstallMethodCommand, ". /etc/os-release")
	pacman := strings.Index(linuxInstallMethodCommand, "command -v pacman")
	if release < 0 || pacman < 0 || pacman < release {
		t.Fatalf("package-manager probe checks executables before /etc/os-release: %q", linuxInstallMethodCommand)
	}
}

func TestLinuxInstallProbeUsesDistroInsteadOfIncidentalExecutables(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		release string
		want    string
	}{
		{name: "Ubuntu ignores pacman", release: "ID=ubuntu\nVERSION_CODENAME=noble\n", want: "MESH_INSTALL_METHOD=apt"},
		{name: "Arch prefers pacman", release: "ID=arch\n", want: "MESH_INSTALL_METHOD=pacman"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			releasePath := directory + "/os-release"
			if err := os.WriteFile(releasePath, []byte(test.release), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"apt-get", "curl", "pacman"} {
				if err := os.WriteFile(directory+"/"+name, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // executable test fixtures
					t.Fatal(err)
				}
			}
			probe := strings.ReplaceAll(linuxInstallMethodCommand, "/etc/os-release", shellQuote(releasePath))
			command := exec.CommandContext(context.Background(), "/bin/sh", "-c", probe) //nolint:gosec // production probe with a test-owned os-release path
			command.Env = []string{"PATH=" + directory}
			output, err := command.CombinedOutput()
			if err != nil || !strings.Contains(string(output), test.want) {
				t.Fatalf("probe output = %q, error = %v, want %q", output, err, test.want)
			}
		})
	}
}

func TestProvisionShellProbesUsePortableSyntax(t *testing.T) {
	t.Parallel()

	for name, script := range map[string]string{
		"brew":            brewPathProbeCommand,
		"linux installer": linuxInstallMethodCommand,
		"sudo":            sudoCapabilityProbeCommand,
		"tailscale":       tailscaleStatusCommand,
	} {
		t.Run(name, func(t *testing.T) {
			command := exec.CommandContext(context.Background(), "/bin/sh", "-n", "-c", script) //nolint:gosec // scripts are fixed production constants
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("shell syntax error: %v: %s", err, output)
			}
		})
	}
}

func runningStatus(name, address string) []byte {
	return []byte(fmt.Sprintf(`{"BackendState":"Running","Self":{"DNSName":%q,"HostName":"fixture","TailscaleIPs":[%q]}}`, name+".", address))
}

func formatEvents(events []Event) string {
	var text strings.Builder
	for _, event := range events {
		fmt.Fprintf(&text, "%s %s\n", event.Step, event.Detail)
	}
	return text.String()
}

func assertReaderContents(t *testing.T, reader io.Reader, want string) {
	t.Helper()
	if reader == nil {
		t.Fatalf("stdin is nil, want %q", want)
	}
	contents, err := io.ReadAll(reader)
	if err != nil || string(contents) != want {
		t.Fatalf("stdin = %q, %v, want %q", contents, err, want)
	}
}

func assertOutputLimits(t *testing.T, got []remoteOutputLimits, stdout int) {
	t.Helper()
	want := []remoteOutputLimits{{Stdout: stdout}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("output limits = %#v, want %#v", got, want)
	}
}

func noninteractiveSudoProbe() []byte {
	return []byte("MESH_SUDO_MODE=noninteractive\nMESH_SUDO_PATH=/usr/bin/sudo\n")
}

func noninteractiveSudoSpec() privilegeSpec {
	return privilegeSpec{Mode: privilegeSudoNonInteractive, SudoPath: "/usr/bin/sudo"}
}

func noninteractiveSudoCommand(command string) string {
	return noninteractiveSudoSpec().command(command)
}

func TestRequestAuthKeyAsksInsteadOfFailing(t *testing.T) {
	t.Parallel()

	asked := 0
	request := provisionRequest{
		Target: "pc (shaul@10.0.0.4)",
		AuthKeyPrompt: func(_ context.Context, target string) ([]byte, error) {
			asked++
			if target != "pc (shaul@10.0.0.4)" {
				t.Errorf("prompt target = %q", target)
			}
			return []byte("  tskey-auth-example  \n"), nil
		},
	}
	key, err := requestAuthKey(context.Background(), &request, tailscaleNeedsLogin)
	if err != nil {
		t.Fatalf("requestAuthKey() error = %v", err)
	}
	if string(key) != "tskey-auth-example" {
		t.Fatalf("key = %q, want the pasted key trimmed", key)
	}
	if asked != 1 {
		t.Fatalf("prompt asked %d times, want 1", asked)
	}
}

func TestRequestAuthKeyWithoutAPromptStillDiagnoses(t *testing.T) {
	t.Parallel()

	// An unattended run has nobody to ask, and must keep naming the flag.
	request := provisionRequest{Target: "pc"}
	_, err := requestAuthKey(context.Background(), &request, tailscaleNeedsLogin)
	assertDiagnosticCode(t, err, DiagnosticTailscaleLoggedOut)
}

func TestRequestAuthKeyRejectsAnEmptyPaste(t *testing.T) {
	t.Parallel()

	// Enter on an empty prompt must not be read as a key.
	request := provisionRequest{
		Target:        "pc",
		AuthKeyPrompt: func(context.Context, string) ([]byte, error) { return []byte("   \n"), nil },
	}
	_, err := requestAuthKey(context.Background(), &request, tailscaleNeedsLogin)
	assertDiagnosticCode(t, err, DiagnosticTailscaleLoggedOut)
}
