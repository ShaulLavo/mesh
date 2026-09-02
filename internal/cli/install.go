package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/muesli/cancelreader"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/tailnet"
	installscript "github.com/shaul/mesh/scripts/install"
)

// daemonInstallCommand installs this machine's own daemon. `mesh add` installs
// one on a host it adopts, but a Mesh installed from a package manager has only
// the binary, so the machine can drive other hosts and host nothing itself.
func (a *application) daemonInstallCommand() *cobra.Command {
	var (
		yes           bool
		daemonPort    uint16 = bootstrap.DefaultPort
		sshPort       uint16 = bootstrap.DefaultSSHPort
		webSocketPath        = bootstrap.DefaultWebSocketPath
	)
	command := &cobra.Command{
		Use:   "install",
		Short: "Install and start this machine's Mesh daemon",
		Long: "Install the service that runs this machine's daemon, so it can host\n" +
			"sessions and be adopted by another host. Installing Mesh from a package\n" +
			"manager gives you the command but no daemon.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.installLocalDaemon(cmd, yes, daemonPort, sshPort, webSocketPath)
		},
	}
	command.Flags().BoolVar(&yes, "yes", false, "install without asking")
	return command
}

func (a *application) installLocalDaemon(cmd *cobra.Command, yes bool, daemonPort, sshPort uint16, webSocketPath string) error {
	script, ok := installscript.Script(runtime.GOOS)
	if !ok {
		return fmt.Errorf("mesh has no installer for %s", runtime.GOOS)
	}
	service, err := installscript.RenderService(runtime.GOOS, installscript.ServiceOptions{
		DaemonPort:    daemonPort,
		SSHPort:       sshPort,
		WebSocketPath: webSocketPath,
	})
	if err != nil {
		return fmt.Errorf("render the Mesh service: %w", err)
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return err
	}
	host, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		return err
	}
	publicKey, err := ssh.NewPublicKey(host.PublicKey)
	if err != nil {
		return fmt.Errorf("encode this host's public key: %w", err)
	}
	authorizedKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey)))

	if !yes {
		approved, err := a.confirmInstall(cmd, daemonPort, sshPort)
		if err != nil {
			return err
		}
		if !approved {
			return errors.New("installation was declined")
		}
	}

	staged, err := stageOwnBinary()
	if err != nil {
		return err
	}
	defer os.Remove(staged) //nolint:errcheck // the installer removes it on success

	run := exec.CommandContext(cmd.Context(), "/bin/sh", "-s", "--",
		staged,
		strconv.Itoa(int(daemonPort)),
		strconv.Itoa(int(sshPort)),
		webSocketPath,
		base64.StdEncoding.EncodeToString([]byte(authorizedKey)),
		base64.StdEncoding.EncodeToString([]byte(service)),
	)
	run.Stdin = strings.NewReader(script)
	run.Stdout, run.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := run.Run(); err != nil {
		return fmt.Errorf("install the Mesh service: %w", err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "installed the Mesh daemon; this machine can host sessions now\n")
	return err
}

// stageOwnBinary copies this executable somewhere disposable. The installer
// removes the file it is given, which is right for a binary uploaded over SSH
// and would delete the installed Mesh if it were handed the real path.
func stageOwnBinary() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the running Mesh binary: %w", err)
	}
	source, err := os.Open(executable)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", executable, err)
	}
	defer source.Close() //nolint:errcheck // read-only

	staged, err := os.CreateTemp("", "mesh-install-*")
	if err != nil {
		return "", fmt.Errorf("stage the Mesh binary: %w", err)
	}
	if _, err := io.Copy(staged, source); err != nil {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
		return "", fmt.Errorf("stage the Mesh binary: %w", err)
	}
	if err := staged.Chmod(0o755); err != nil {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
		return "", fmt.Errorf("stage the Mesh binary: %w", err)
	}
	if err := staged.Close(); err != nil {
		_ = os.Remove(staged.Name())
		return "", fmt.Errorf("stage the Mesh binary: %w", err)
	}
	return staged.Name(), nil
}

func (a *application) confirmInstall(cmd *cobra.Command, daemonPort, sshPort uint16) (bool, error) {
	input, output := a.dependencies.Stdin, cmd.ErrOrStderr()
	if input == nil || !term.IsTerminal(input.Fd()) {
		return false, errors.New("installing needs an interactive terminal, or pass --yes")
	}
	where := "a systemd user service"
	if runtime.GOOS == "darwin" {
		where = "a launchd agent"
	}
	stateDir, _ := paths.StateDir()
	if _, err := fmt.Fprintf(output,
		"\nInstall the Mesh daemon on this machine?\n\n"+
			"  1. copy mesh to %s\n"+
			"  2. install %s that starts it at login\n"+
			"  3. listen for Mesh on port %d and SSH on port %d, on this machine's Tailscale addresses only\n"+
			"  4. keep session state in %s\n\n",
		filepath.Join(homeOrTilde(), ".local", "bin", "mesh"), where, daemonPort, sshPort, stateDir); err != nil {
		return false, err
	}
	if _, err := fmt.Fprint(output, "Continue? [y/N] "); err != nil {
		return false, err
	}
	reader, err := cancelreader.NewReader(input)
	if err != nil {
		return false, fmt.Errorf("prepare the installation prompt: %w", err)
	}
	defer reader.Close() //nolint:errcheck // prompt result is authoritative
	stop := context.AfterFunc(cmd.Context(), func() { reader.Cancel() })
	answer, err := bufio.NewReader(reader).ReadString('\n')
	stop()
	if cmd.Context().Err() != nil {
		return false, cmd.Context().Err()
	}
	if errors.Is(err, cancelreader.ErrCanceled) {
		return false, context.Canceled
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func homeOrTilde() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return "~"
}

// daemonWithInstall groups install under daemon: mesh serve already owns the
// word service in this product, and mesh install would read as installing Mesh
// itself rather than its daemon.
func daemonWithInstall(app *application) *cobra.Command {
	daemon := app.daemonCommand()
	daemon.AddCommand(app.daemonInstallCommand())
	return daemon
}

// addTargetUsage answers "which host?" with machines this one can actually
// reach: the Host aliases in ~/.ssh/config, which adoption resolves directly,
// and the tailnet peers, which a target has to be on anyway. Hosts already
// adopted are named so the list does not invite adopting one twice.
func (a *application) addTargetUsage(cmd *cobra.Command) error {
	usage := &usageError{
		problem: cmd.CommandPath() + " needs an SSH target",
		example: "mesh add user@host",
	}
	hosts, _ := LoadHosts()
	adopted := make(map[string]string, len(hosts)*2)
	for _, host := range hosts {
		adopted[host.Alias] = host.Alias
		for _, address := range host.Addresses {
			adopted[address] = host.Alias
		}
	}

	var first string
	for _, entry := range sshConfigHosts() {
		line := fmt.Sprintf("  %-20s %s", SafeTerminalText(entry.Alias), SafeTerminalText(entry.HostName))
		if alias, ok := adopted[entry.Alias]; ok {
			line += "   already added as " + SafeTerminalText(alias)
		} else if first == "" {
			first = entry.Alias
		}
		if len(usage.details) == 0 {
			usage.details = append(usage.details, "from ~/.ssh/config:")
		}
		usage.details = append(usage.details, line)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), defaultCatalogTimeout)
	defer cancel()
	if peers, err := tailnet.Peers(ctx); err == nil {
		var tailnetLines []string
		for _, peer := range peers {
			if len(peer.Addrs) == 0 {
				continue
			}
			name := peer.Name
			if index := strings.Index(name, "."); index > 0 {
				name = name[:index]
			}
			line := fmt.Sprintf("  %-20s %s", SafeTerminalText(name), peer.Addrs[0])
			if alias, ok := adopted[peer.Addrs[0]]; ok {
				line += "   already added as " + SafeTerminalText(alias)
			} else if !peer.Online {
				line += "   offline"
			}
			tailnetLines = append(tailnetLines, line)
		}
		if len(tailnetLines) > 0 {
			usage.details = append(usage.details, "on your tailnet:")
			usage.details = append(usage.details, tailnetLines...)
		}
	}

	// Suggest something real when there is one, rather than a placeholder.
	if first != "" {
		usage.example = "mesh add " + first
	}
	return usage
}
