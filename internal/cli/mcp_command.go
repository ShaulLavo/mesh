package cli

import (
	"fmt"
	"log"
	"strings"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/spf13/cobra"
)

func (a *application) mcpCommand() *cobra.Command {
	var identityName string
	var httpAddr string
	var allowOrigins []string
	var tokenFile string
	command := &cobra.Command{
		Use:   "mcp",
		Short: "Run the stateless native MCP server (stdio or loopback HTTP)",
		Long: "Run Mesh's stateless MCP 2026-07-28 server over stdio, or over loopback\n" +
			"Streamable HTTP when --http is passed.\n\n" +
			"In stdio mode, stdout carries only newline-delimited JSON-RPC MCP messages;\n" +
			"diagnostics and startup errors go to stderr. Pass --identity NAME to enable\n" +
			"durable execution contexts and mesh_exec; without --identity, only passive\n" +
			"native discovery is enabled.\n\n" +
			"In HTTP mode (--http 127.0.0.1:PORT --identity NAME), Mesh binds only the\n" +
			"specified loopback address at /mcp and authenticates every POST with a\n" +
			"random bearer token stored in a private 0600 file. Rotate tokens with\n" +
			"`mesh mcp token rotate`.",
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
			if cmd.Flags().Changed("http") {
				return a.runMCPHTTP(cmd, mcpHTTPOptions{
					ListenAddr:     httpAddr,
					IdentityName:   identityName,
					AllowedOrigins: allowOrigins,
					TokenFile:      tokenFile,
				})
			}
			if len(allowOrigins) > 0 || cmd.Flags().Changed("token-file") {
				return agentapi.Usage("mcp.serve", "--allow-origin and --token-file require --http.")
			}
			return a.runMCPStdio(cmd, cmd.Flags().Changed("identity"), identityName)
		},
	}
	// Route all Cobra help and usage text for `mesh mcp` to stderr so stdout
	// stays strictly protocol-only.
	command.SetOut(a.dependencies.Stderr)
	command.SetErr(a.dependencies.Stderr)
	command.Flags().StringVar(&identityName, "identity", "", "local application identity created with mesh mcp identity create NAME")
	command.Flags().StringVar(&httpAddr, "http", "", "bind the native Streamable HTTP server on one loopback host:port (for example 127.0.0.1:8765)")
	command.Flags().StringArrayVar(&allowOrigins, "allow-origin", nil, "allow an exact loopback HTTP Origin header (repeatable; requires --http)")
	command.Flags().StringVar(&tokenFile, "token-file", "", "private 0600 file path for the HTTP bearer token record (requires --http)")
	command.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return agentapi.Usage("mcp.serve", err.Error())
	})
	command.AddCommand(a.mcpIdentityCommand(), a.mcpTokenCommand())
	command.AddCommand(a.mcpServiceCommands()...)
	return command
}

func (a *application) runMCPStdio(cmd *cobra.Command, hasIdentity bool, identityName string) error {
	var caller protocol.CallerCredential
	if hasIdentity {
		if strings.TrimSpace(identityName) == "" {
			return agentapi.Usage("mcp.serve", "--identity requires a non-empty identity name.")
		}
		id, err := loadApplicationIdentity("mcp.serve", identityName)
		if err != nil {
			return err
		}
		caller = protocol.CallerCredential(id.Credential.Encode())
	}
	// Keep all standard library log output off protocol stdout.
	log.SetOutput(cmd.ErrOrStderr())
	pool := newMCPHostPool(a.dependencies.DialPassive, a.dependencies.Now)
	defer pool.Close()
	controls := newMCPControls(a, pool, caller)
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
	if err := server.Serve(cmd.Context(), a.dependencies.Stdin, a.dependencies.Stdout); err != nil {
		return fmt.Errorf("serve MCP stdio: %w", err)
	}
	return nil
}
