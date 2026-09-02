package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/cancelreader"

	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/tui"
)

type bootstrapRunner func(context.Context, bootstrap.Options) (bootstrap.Result, error)

type bootstrapUI struct {
	steps    *cli.StepPrinter
	input    io.Reader
	output   io.Writer
	username func() (string, error)
	terminal func() bool
}

const maximumTailscaleAuthKeyFileBytes = 64 << 10

func commandDependencies() cli.Dependencies {
	return cli.Dependencies{
		Bootstrap: newBootstrapFunc(bootstrap.Run, bootstrapUI{
			input:  os.Stdin,
			output: os.Stderr,
			terminal: func() bool {
				return term.IsTerminal(os.Stdin.Fd())
			},
			username: func() (string, error) {
				current, err := user.Current()
				if err != nil {
					return "", err
				}
				return current.Username, nil
			},
		}),
		Picker: tui.NewCLIPicker(os.Stdin, os.Stdout),
	}
}

func newBootstrapFunc(run bootstrapRunner, ui bootstrapUI) cli.BootstrapFunc {
	return func(ctx context.Context, request cli.AddRequest) (cli.BootstrapResult, error) {
		stateDir, err := paths.StateDir()
		if err != nil {
			return cli.BootstrapResult{}, err
		}
		hosts, err := cli.LoadHosts()
		if err != nil {
			return cli.BootstrapResult{}, err
		}
		steps := cli.NewStepPrinter(ui.output, "BOOTSTRAP")
		defer steps.Done()
		ui.steps = steps
		authKey, err := readTailscaleAuthKey(request.TailscaleAuthKeyFile)
		if err != nil {
			return cli.BootstrapResult{}, err
		}
		defer clear(authKey)
		confirmProvision := provisionPrompt(ui)
		if request.Yes {
			confirmProvision = func(context.Context, bootstrap.ProvisionConfirmation) (bool, error) {
				return true, nil
			}
		}

		result, err := run(ctx, bootstrap.Options{
			Target:                 request.Target,
			StateDir:               stateDir,
			ExpectedIdentity:       identityForAlias(hosts, request.Alias),
			TailscaleAuthKey:       authKey,
			TailscaleAuthKeyPrompt: authKeyPrompt(ui),
			LocalTailscaleSetup:    localTailscaleSetup(ui),
			ConfirmProvision:       confirmProvision,
			SudoPassword:           sudoPasswordPrompt(ui),
			SSH: bootstrap.SSHOptions{
				Password:       passwordPrompt(ui),
				Passphrase:     passphrasePrompt(ui),
				ConfirmHostKey: hostKeyPrompt(ui),
			},
			Progress: func(event bootstrap.Event) {
				steps.Step(string(event.Step), event.Detail)
			},
		})
		if err != nil {
			return cli.BootstrapResult{}, err
		}
		return cli.BootstrapResult{
			Host: cli.HostRecord{
				ID:            result.ID,
				MeshIdentity:  result.MeshIdentity,
				TailscaleName: result.TailscaleName,
				Addresses:     result.TailscaleAddresses,
				Endpoint:      result.Endpoint,
			},
			AlreadyConfigured: result.AlreadyConfigured,
		}, nil
	}
}

func sudoPasswordPrompt(ui bootstrapUI) bootstrap.SudoPasswordFunc {
	return func(ctx context.Context, target string) ([]byte, error) {
		if ui.terminal == nil || !ui.terminal() {
			return nil, errors.New("sudo authentication needs an interactive terminal, a root SSH login, or passwordless sudo")
		}
		password, err := promptSecret(ctx, ui, "sudo password", "Password for "+target)
		if err != nil {
			return nil, err
		}
		return []byte(password), nil
	}
}

// authKeyPrompt asks for a Tailscale auth key at the moment the remote host
// turns out to need one, so an interactive adoption never has to fail and be
// started again. Without a terminal it says what to pass instead.
func authKeyPrompt(ui bootstrapUI) bootstrap.AuthKeyFunc {
	return func(ctx context.Context, target string) ([]byte, error) {
		if ui.terminal == nil || !ui.terminal() {
			return nil, errors.New("Tailscale needs an auth key; pass --tailscale-auth-key-file, or run mesh add from a terminal to paste one")
		}
		description := target + " has Tailscale but is not logged in.\n\n" +
			"  1. open https://login.tailscale.com/admin/settings/keys\n" +
			"  2. Generate auth key, and turn on Reusable\n" +
			"  3. paste it below"
		key, err := promptSecret(ctx, ui, "Tailscale auth key", description)
		if err != nil {
			return nil, err
		}
		return []byte(key), nil
	}
}

func readTailscaleAuthKey(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path) //nolint:gosec // the CLI flag explicitly selects the local secret file
	if err != nil {
		return nil, fmt.Errorf("open Tailscale auth key file %s: %w", path, err)
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, maximumTailscaleAuthKeyFileBytes+1))
	defer clear(contents)
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read Tailscale auth key file %s: %w", path, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close Tailscale auth key file %s: %w", path, closeErr)
	}
	if len(contents) > maximumTailscaleAuthKeyFileBytes {
		return nil, fmt.Errorf("Tailscale auth key file %s is larger than %d bytes", path, maximumTailscaleAuthKeyFileBytes)
	}
	key := bytes.TrimSpace(contents)
	if len(key) == 0 {
		return nil, fmt.Errorf("Tailscale auth key file %s is empty", path)
	}
	result := append([]byte(nil), key...)
	return result, nil
}

func provisionPrompt(ui bootstrapUI) bootstrap.ConfirmProvisionFunc {
	return func(ctx context.Context, confirmation bootstrap.ProvisionConfirmation) (bool, error) {
		if ctx == nil {
			return false, errors.New("nil Tailscale provisioning confirmation context")
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if ui.terminal == nil || !ui.terminal() {
			return false, errors.New("Tailscale provisioning needs an interactive terminal or --yes")
		}
		ui.pauseSteps()
		if _, err := fmt.Fprintf(ui.output, "\n%s\n\n", cli.SafeTerminalText(confirmation.Summary)); err != nil {
			return false, err
		}
		for i, action := range confirmation.Actions {
			if _, err := fmt.Fprintf(ui.output, "  %d. %s\n     %s\n", i+1, cli.SafeTerminalText(action.Description), cli.SafeTerminalText(action.Command)); err != nil {
				return false, err
			}
		}
		for _, check := range confirmation.Checks {
			if _, err := fmt.Fprintf(ui.output, "\n  first: %s\n", cli.SafeTerminalText(check)); err != nil {
				return false, err
			}
		}
		if _, err := fmt.Fprint(ui.output, "\nContinue? [y/N] "); err != nil {
			return false, err
		}
		reader, err := cancelreader.NewReader(ui.input)
		if err != nil {
			return false, fmt.Errorf("prepare Tailscale provisioning confirmation: %w", err)
		}
		defer reader.Close() //nolint:errcheck // prompt result is authoritative
		stopCancellation := context.AfterFunc(ctx, func() { reader.Cancel() })
		answer, err := bufio.NewReader(reader).ReadString('\n')
		stopCancellation()
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if errors.Is(err, cancelreader.ErrCanceled) {
			return false, context.Canceled
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return false, fmt.Errorf("read Tailscale provisioning confirmation: %w", err)
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		return answer == "y" || answer == "yes", nil
	}
}

func identityForAlias(hosts []cli.HostRecord, alias string) string {
	for _, host := range hosts {
		if strings.EqualFold(host.Alias, alias) {
			return host.MeshIdentity
		}
	}
	return ""
}

func passwordPrompt(ui bootstrapUI) func(context.Context, string) (string, error) {
	return func(ctx context.Context, target string) (string, error) {
		return promptSecret(ctx, ui, "SSH password", "Password for "+target)
	}
}

func passphrasePrompt(ui bootstrapUI) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, identityFile string) ([]byte, error) {
		value, err := promptSecret(ctx, ui, "SSH key passphrase", identityFile)
		return []byte(value), err
	}
}

func promptSecret(ctx context.Context, ui bootstrapUI, title, description string) (string, error) {
	ui.pauseSteps()
	var value string
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title(title).
			Description(description).
			EchoMode(huh.EchoModePassword).
			Value(&value),
	)).WithInput(ui.input).WithOutput(ui.output)
	if err := form.RunWithContext(ctx); err != nil {
		return "", err
	}
	return value, nil
}

func hostKeyPrompt(ui bootstrapUI) func(context.Context, bootstrap.HostKey) (bool, error) {
	return func(ctx context.Context, key bootstrap.HostKey) (bool, error) {
		ui.pauseSteps()
		var accepted bool
		form := huh.NewForm(huh.NewGroup(
			huh.NewConfirm().
				Title("Trust this SSH host key?").
				Description(fmt.Sprintf("%s\n%s %s", key.Host, key.Algorithm, key.Fingerprint)).
				Affirmative("Trust").
				Negative("Cancel").
				Value(&accepted),
		)).WithInput(ui.input).WithOutput(ui.output)
		if err := form.RunWithContext(ctx); err != nil {
			return false, err
		}
		return accepted, nil
	}
}

// pauseSteps stops the live step line before a prompt takes the terminal.
func (u bootstrapUI) pauseSteps() {
	if u.steps != nil {
		u.steps.Pause()
	}
}
