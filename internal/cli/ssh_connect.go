package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/shaul/mesh/internal/terminal"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/charmbracelet/x/term"
	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/sshd"
	"github.com/shaul/mesh/internal/terminfo"
)

func (a *application) sshCommand() *cobra.Command {
	var port uint
	var request sshd.RunRequest
	var noPTY bool
	command := &cobra.Command{
		Use:   "ssh HOST [-- command...]",
		Short: "Open a shell or command that ends when the connection closes",
		Long:  "Run directly through the host's private Mesh SSH listener.\nThis connection has no saved session, detach key, or reconnection.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if port == 0 || port > 65535 {
				return errors.New("SSH port must be between 1 and 65535")
			}
			request.Args = args[1:]
			return a.executeSSH(cmd.Context(), args[0], uint16(port), request, noPTY) //nolint:gosec // port checked above
		},
	}
	command.Flags().UintVar(&port, "ssh-port", uint(bootstrap.DefaultSSHPort), "Mesh SSH listener port")
	command.Flags().StringVar(&request.Cwd, "cwd", "", "Remote working directory; relative paths start at the account home")
	command.Flags().StringArrayVar(&request.Env, "env", nil, "Remote KEY=VALUE override; repeatable, last value wins")
	command.Flags().BoolVarP(&noPTY, "no-pty", "T", false, "Use separate byte-preserving stdout and stderr without a terminal")
	return command
}

func (a *application) executeSSH(ctx context.Context, target string, port uint16, request sshd.RunRequest, noPTY bool) error {
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	host, err := resolveHostTarget(hosts, target)
	if err != nil {
		return err
	}
	if err := verifyNamedHost(ctx, host, a.dependencies.DialControl); err != nil {
		return err
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return fmt.Errorf("locate SSH device identity: %w", err)
	}
	if noPTY || !term.IsTerminal(a.dependencies.Stdin.Fd()) {
		return a.runPipeSSH(ctx, host, stateDir, port, request)
	}
	client, err := dialMeshSSH(ctx, host, stateDir, port)
	if err != nil {
		return err
	}
	defer client.Close() //nolint:errcheck // remote command status decides success
	return a.runSSHRequest(ctx, client, request)
}

func (a *application) runSSHRequest(ctx context.Context, client *ssh.Client, request sshd.RunRequest) error {
	channel, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("open SSH channel: %w", err)
	}
	defer channel.Close() //nolint:errcheck // Wait owns the result
	stop := context.AfterFunc(ctx, func() { _ = channel.Close() })
	defer stop()
	terminal, closeTerminal, interactive, err := localAttachTerminal(ctx, a.dependencies.Stdin, a.dependencies.Stdout)
	if err != nil {
		return err
	}
	defer closeTerminal()
	input, err := channel.StdinPipe()
	if err != nil {
		return fmt.Errorf("open SSH input: %w", err)
	}
	channel.Stdout, channel.Stderr = terminal.Output, a.dependencies.Stderr
	if interactive {
		name := os.Getenv("TERM")
		if name == "" {
			name = "xterm-256color"
		}
		request.Terminfo = terminfo.Capture(ctx, name)
		if err := channel.RequestPty(name, terminal.Size.Rows, terminal.Size.Cols, ssh.TerminalModes{}); err != nil {
			return fmt.Errorf("request SSH terminal: %w", err)
		}
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode SSH command: %w", err)
	}
	if err := channel.Start("run " + string(payload)); err != nil {
		return fmt.Errorf("start SSH command: %w", err)
	}
	if interactive {
		if err := terminal.begin(); err != nil {
			return err
		}
	}
	inputDone := make(chan struct{})
	go func() { defer close(inputDone); _, _ = io.Copy(input, terminal.Input); _ = input.Close() }()
	defer func() { terminal.CancelInput(); <-inputDone }()
	relayContext, cancel := context.WithCancel(ctx)
	defer cancel()
	relayDone := make(chan struct{})
	go func() { defer close(relayDone); relaySSHEvents(relayContext, channel, terminal.Resizes) }()
	defer func() { cancel(); <-relayDone }()
	err = channel.Wait()
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return statusError{code: exit.ExitStatus()}
	}
	if err != nil {
		return fmt.Errorf("SSH connection ended: %w", err)
	}
	return nil
}

func relaySSHEvents(ctx context.Context, channel *ssh.Session, resizes <-chan terminal.Size) {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case size := <-resizes:
			_ = channel.WindowChange(size.Rows, size.Cols)
		case value := <-signals:
			names := map[os.Signal]ssh.Signal{syscall.SIGINT: ssh.SIGINT, syscall.SIGTERM: ssh.SIGTERM, syscall.SIGHUP: ssh.SIGHUP}
			_ = channel.Signal(names[value])
		}
	}
}
