package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/appcaller"
	"github.com/spf13/cobra"
)

// mcpCommand is the group for agent frontends. The native MCP server joins
// it as the group's own action.
func (a *application) mcpCommand() *cobra.Command {
	group := &cobra.Command{Use: "mcp", Short: "Configure agent frontends for Mesh's MCP tools", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	}}
	identity := &cobra.Command{Use: "identity", Short: "Manage per-agent application identities", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	}}
	identity.AddCommand(a.mcpIdentityCreateCommand(), a.mcpIdentityListCommand())
	group.AddCommand(identity)
	return group
}

// meshConfigDir is the directory holding hosts.json and identities.
func meshConfigDir() (string, error) {
	path, err := ConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

func (a *application) mcpIdentityCreateCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "create NAME", Short: "Create a private application credential for one agent frontend", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := meshConfigDir()
		if err != nil {
			return err
		}
		created, err := appcaller.Create(dir, args[0], time.Now())
		if errors.Is(err, appcaller.ErrIdentityExists) {
			return agentapi.Usage("mcp.identity.create", fmt.Sprintf("Identity %s already exists. Pick a new name for a new identity.", args[0]))
		}
		if err != nil {
			return agentapi.Usage("mcp.identity.create", err.Error())
		}
		summary := agentapi.ApplicationIdentity{Name: created.Name, Fingerprint: created.Credential.Fingerprint(), CreatedAt: created.CreatedAt, Path: filepath.Join(appcaller.Dir(dir), created.Name+".json")}
		if asJSON {
			return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: summary})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Created identity %s (%s) at %s\nConfigure the agent with: mesh mcp --identity %s\n", summary.Name, summary.Fingerprint, summary.Path, summary.Name)
		return executionBoundaryError("print identity", err)
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	return cmd
}

func (a *application) mcpIdentityListCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "ls", Aliases: []string{agentSessionListCommand}, Short: "List local application identities without their secrets", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := meshConfigDir()
		if err != nil {
			return err
		}
		list, err := appcaller.List(dir)
		if err != nil {
			return executionBoundaryError("list identities", err)
		}
		out := agentapi.ApplicationIdentities{Identities: make([]agentapi.ApplicationIdentity, 0, len(list))}
		for _, s := range list {
			out.Identities = append(out.Identities, agentapi.ApplicationIdentity{Name: s.Name, Fingerprint: s.Fingerprint, CreatedAt: s.CreatedAt, Path: s.Path})
		}
		if asJSON {
			return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: out})
		}
		for _, s := range out.Identities {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", s.Name, s.Fingerprint, s.CreatedAt.Format(time.RFC3339)); err != nil {
				return executionBoundaryError("print identities", err)
			}
		}
		return nil
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	return cmd
}

// loadApplicationIdentity reads a named identity for a control.
func loadApplicationIdentity(operation, name string) (appcaller.Identity, error) {
	dir, err := meshConfigDir()
	if err != nil {
		return appcaller.Identity{}, err
	}
	identity, err := appcaller.Load(dir, name)
	if err != nil {
		return appcaller.Identity{}, agentapi.Usage(operation, fmt.Sprintf("%v. Create it with mesh mcp identity create %s.", err, name))
	}
	return identity, nil
}
