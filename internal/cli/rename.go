package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/muesli/cancelreader"
	"github.com/spf13/cobra"
)

// renameCommand changes the local name for an adopted host. The name is this
// machine's label for it and nothing on the host depends on it, so renaming is
// a local edit rather than anything the remote needs to hear about.
func (a *application) renameCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "rename host new-name",
		Aliases: []string{"mv"},
		Short:   "Change the local name for an adopted host",
		Args:    exactArgs(2, "the host to rename and its new name", "mesh rename omarchy pc"),
		RunE: func(cmd *cobra.Command, args []string) error {
			renamed, err := RenameHost(args[0], args[1])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "renamed %s to %s\n", args[0], renamed.Alias)
			return err
		},
	}
}

// RenameHost gives an adopted host a new local name.
func RenameHost(from, to string) (HostRecord, error) {
	alias, err := ValidateHostAlias(to)
	if err != nil {
		return HostRecord{}, err
	}
	hosts, err := LoadHosts()
	if err != nil {
		return HostRecord{}, err
	}
	current, err := hostWithAlias(hosts, from)
	if err != nil {
		return HostRecord{}, err
	}
	if strings.EqualFold(current.Alias, alias) {
		return current, nil
	}
	for _, existing := range hosts {
		if strings.EqualFold(existing.Alias, alias) {
			return HostRecord{}, fmt.Errorf("host name %q already belongs to host %s", alias, existing.ID)
		}
	}
	// Remove the old entry first: SaveHost matches on identity, and would
	// otherwise leave the host under its previous name too.
	remaining := make([]HostRecord, 0, len(hosts))
	for _, existing := range hosts {
		if existing.ID != current.ID {
			remaining = append(remaining, existing)
		}
	}
	current.Alias = alias
	remaining = append(remaining, current)
	sortHosts(remaining)
	if err := writeHostConfig(hostConfig{Version: hostConfigVersion, Hosts: remaining}); err != nil {
		return HostRecord{}, err
	}
	return current, nil
}

// existingAliasFor reports the name an adopted host already carries, when the
// adoption that just ran would give it a different one.
func existingAliasFor(id, proposed string) string {
	hosts, err := LoadHosts()
	if err != nil {
		return ""
	}
	for _, existing := range hosts {
		if existing.ID == id && !strings.EqualFold(existing.Alias, proposed) {
			return existing.Alias
		}
	}
	return ""
}

// confirmRename asks before replacing a name the operator chose earlier.
// Adopting a host again under a different name used to rename it silently, so
// `mesh add omarchy` quietly cost someone the alias `pc` they had been using.
func (a *application) confirmRename(cmd *cobra.Command, current, proposed string) (bool, error) {
	input := a.dependencies.Stdin
	if input == nil || !term.IsTerminal(input.Fd()) {
		// Nobody to ask: keep the name already in use.
		return false, nil
	}
	output := cmd.ErrOrStderr()
	if _, err := fmt.Fprintf(output, "\nThat host is already added as %q.\nRename it to %q? [y/N] ",
		SafeTerminalText(current), SafeTerminalText(proposed)); err != nil {
		return false, err
	}
	reader, err := cancelreader.NewReader(input)
	if err != nil {
		return false, fmt.Errorf("prepare the rename prompt: %w", err)
	}
	defer reader.Close() //nolint:errcheck // prompt result is authoritative
	stop := context.AfterFunc(cmd.Context(), func() { reader.Cancel() })
	answer, err := bufio.NewReader(reader).ReadString('\n')
	stop()
	if cmd.Context().Err() != nil {
		return false, cmd.Context().Err()
	}
	if err != nil && !errors.Is(err, cancelreader.ErrCanceled) && answer == "" {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
