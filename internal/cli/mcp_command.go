package cli

import (
	"fmt"
	"log"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/release"
	"github.com/spf13/cobra"
)

func (a *application) mcpCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "mcp",
		Short: "Run the stateless native MCP stdio server",
		Long: "Run Mesh's stateless MCP 2026-07-28 server over stdio.\n" +
			"stdout carries only newline-delimited JSON-RPC MCP messages;\n" +
			"diagnostics and startup errors go to stderr.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return agentapi.Usage("mcp.serve", fmt.Sprintf("mesh mcp takes no positional arguments (got %q).", args[0]))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if private, err := cmd.Flags().GetBool("privacy"); err == nil && private {
				return agentapi.Usage("mcp.serve", "Privacy mode cannot be combined with mesh mcp.")
			}
			// Keep all standard library log output off protocol stdout.
			log.SetOutput(cmd.ErrOrStderr())
			pool := newMCPHostPool(a.dependencies.DialPassive, a.dependencies.Now)
			defer pool.Close()
			controls := newMCPControls(a, pool)
			server, err := mcpnative.NewStdioServer(mcpnative.ServerOptions{
				Version:    release.Metadata().Version,
				Controls:   controls,
				Stderr:     cmd.ErrOrStderr(),
				OnShutdown: pool.Close,
			})
			if err != nil {
				return fmt.Errorf("initialize MCP stdio server: %w", err)
			}
			// Protocol messages always go to the real stdout stream, while Cobra
			// help/usage output is routed to stderr so non-protocol text never
			// touches stdout.
			return server.Serve(cmd.Context(), a.dependencies.Stdin, a.dependencies.Stdout)
		},
	}
	// Route all Cobra help and usage text for `mesh mcp` to stderr so stdout
	// stays strictly protocol-only.
	command.SetOut(a.dependencies.Stderr)
	command.SetErr(a.dependencies.Stderr)
	command.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return agentapi.Usage("mcp.serve", err.Error())
	})
	command.AddCommand(a.mcpIdentityCommand())
	return command
}
