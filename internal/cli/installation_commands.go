package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	meshdaemon "github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/spf13/cobra"
)

func installationCommandLines(copies []updateinstall.CommandCopy, mask *privacy.Mask) []string {
	var lines []string
	for _, command := range copies {
		path := SafeTerminalText(mask.Text(command.Path))
		lines = append(lines, fmt.Sprintf("Separate Mesh command: %s. Mesh updates do not maintain this copy; shells that find it first run it.", path), "Fix: "+command.Remedy())
	}
	return lines
}

func printInstallationCommands(output io.Writer, managed string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locate home for installation check: %w", err)
	}
	copies := updateinstall.OtherCommands(managed, updateinstall.CommandSearchDirs(home, os.Getenv("PATH")))
	return printCommandCopies(output, copies, nil)
}

func printCommandCopies(output io.Writer, copies []updateinstall.CommandCopy, mask *privacy.Mask) error {
	lines := installationCommandLines(copies, mask)
	if len(lines) == 0 {
		return nil
	}
	return writeUpdateLines(output, lines)
}

func installationCheckCommand() *cobra.Command {
	var kind, domain string
	command := &cobra.Command{
		Use: "installation-check", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkInstalledHelper(cmd.Context(), kind, domain); err != nil {
				return err
			}
			executable, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate command for installation check: %w", err)
			}
			return printInstallationCommands(cmd.OutOrStdout(), executable)
		},
	}
	command.Flags().StringVar(&kind, "service-kind", "", "verify this daemon service manager's helper")
	command.Flags().StringVar(&domain, "domain", "", "daemon launchd domain")
	return command
}

func runManagedDaemon(ctx context.Context, cfg meshdaemon.Config) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate daemon command: %w", err)
	}
	if err := os.Setenv("PATH", updateinstall.CommandPath(executable, os.Getenv("PATH"))); err != nil {
		return fmt.Errorf("set daemon command PATH: %w", err)
	}
	if err := meshdaemon.Run(ctx, cfg); err != nil {
		return fmt.Errorf("run daemon: %w", err)
	}
	return nil
}

func localCommandCopies() []updateinstall.CommandCopy {
	executable, _ := os.Executable()
	home, _ := os.UserHomeDir()
	return updateinstall.OtherCommands(executable, updateinstall.CommandSearchDirs(home, os.Getenv("PATH")))
}
