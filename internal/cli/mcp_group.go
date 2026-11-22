package cli

import "github.com/spf13/cobra"

// mcpCommand groups the MCP commands. The application-identity commands
// (M3) and the native frontend (M4) join this group.
func (a *application) mcpCommand() *cobra.Command {
	group := &cobra.Command{Use: "mcp", Short: "Host MCP servers on this machine", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	}}
	group.AddCommand(a.mcpServiceCommands()...)
	return group
}
