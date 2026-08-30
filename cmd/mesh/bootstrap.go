package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	"charm.land/huh/v2"

	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/paths"
)

type bootstrapRunner func(context.Context, bootstrap.Options) (bootstrap.Result, error)

type bootstrapUI struct {
	input    io.Reader
	output   io.Writer
	username func() (string, error)
}

func commandDependencies() cli.Dependencies {
	return cli.Dependencies{Bootstrap: newBootstrapFunc(bootstrap.Run, bootstrapUI{
		input:  os.Stdin,
		output: os.Stderr,
		username: func() (string, error) {
			current, err := user.Current()
			if err != nil {
				return "", err
			}
			return current.Username, nil
		},
	})}
}

func newBootstrapFunc(run bootstrapRunner, ui bootstrapUI) cli.BootstrapFunc {
	return func(ctx context.Context, request cli.AddRequest) (cli.BootstrapResult, error) {
		target, err := targetWithUser(request.Target, ui.username)
		if err != nil {
			return cli.BootstrapResult{}, err
		}
		stateDir, err := paths.StateDir()
		if err != nil {
			return cli.BootstrapResult{}, err
		}
		hosts, err := cli.LoadHosts()
		if err != nil {
			return cli.BootstrapResult{}, err
		}

		result, err := run(ctx, bootstrap.Options{
			Target:           target,
			StateDir:         stateDir,
			ExpectedIdentity: identityForAlias(hosts, request.Alias),
			SSH: bootstrap.SSHOptions{
				Password:       passwordPrompt(ui),
				Passphrase:     passphrasePrompt(ui),
				ConfirmHostKey: hostKeyPrompt(ui),
			},
			Progress: func(event bootstrap.Event) {
				fmt.Fprintf(ui.output, "bootstrap %-8s %s\n", event.Step, event.Detail)
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

func targetWithUser(target string, username func() (string, error)) (string, error) {
	if strings.Contains(target, "@") {
		return target, nil
	}
	name, err := username()
	if err != nil {
		return "", fmt.Errorf("resolve local SSH user: %w", err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("resolve local SSH user: username is empty")
	}
	return name + "@" + target, nil
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
