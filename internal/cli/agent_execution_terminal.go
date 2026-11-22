package cli

import (
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/spf13/cobra"
)

// sessionResizeCommand and sessionScreenCommand are the CLI half of the
// terminal job controls mesh mcp offers. They reach only keyed terminal jobs;
// a person's own session is never a keyed job.
func (a *application) sessionResizeCommand() *cobra.Command {
	return a.sessionTerminalCommand("resize", protocol.TypeExecutionResize, "Resize a keyed terminal job while no person is attached")
}

func (a *application) sessionScreenCommand() *cobra.Command {
	return a.sessionTerminalCommand("screen", protocol.TypeExecutionScreen, "Read a keyed terminal job's rendered screen without resizing it")
}

func (a *application) sessionTerminalCommand(name, kind, short string) *cobra.Command {
	o := &executionControlOptions{}
	var cols, rows int
	operation := "sessions." + name
	command := &cobra.Command{Use: name + " SESSION_ID", Short: short, Args: exactExecutionArgs(operation), RunE: func(cmd *cobra.Command, args []string) error {
		if err := o.validateControl(operation, args[0], kind); err != nil {
			return err
		}
		request := protocol.Control{Type: kind, SessionID: args[0], Instance: o.instance}
		if err := terminalCommandSize(operation, kind, cols, rows, &request); err != nil {
			return err
		}
		return a.runAgentExecution(cmd, o.agentExecutionOptions, request)
	}}
	command.Flags().StringVar(&o.host, "host", "", "online machine name or exact host ID, required")
	command.Flags().BoolVar(&o.json, "json", false, "write one agent result, required")
	command.Flags().DurationVar(&o.timeout, "timeout", 5*time.Second, "finite deadline from 100ms to 60s")
	command.Flags().StringVar(&o.instance, "instance", "", "exact instance from the creation receipt, required")
	applicationExecutionFlags(command, &o.agentExecutionOptions)
	if kind == protocol.TypeExecutionResize {
		command.Flags().IntVar(&cols, "cols", 0, "terminal width, required")
		command.Flags().IntVar(&rows, "rows", 0, "terminal height, required")
	} else {
		command.Flags().IntVar(&cols, "cols", 80, "preview width up to 160")
		command.Flags().IntVar(&rows, "rows", 24, "preview height up to 24")
	}
	return command
}

func terminalCommandSize(operation, kind string, cols, rows int, request *protocol.Control) error {
	if kind == protocol.TypeExecutionScreen {
		if err := protocol.ValidateInspectDimensions(cols, rows); err != nil {
			return agentapi.Usage(operation, "Use --cols 1 to 160 and --rows 1 to 24.")
		}
		request.PreviewCols, request.PreviewRows = cols, rows
		return nil
	}
	if cols < 1 || rows < 1 || cols > 2048 || rows > 2048 || cols > (256<<10)/rows {
		return agentapi.Usage(operation, "Pass --cols and --rows from 1 to 2048 with at most 262144 cells.")
	}
	request.Cols, request.Rows = cols, rows
	return nil
}

// terminalAgentData builds resize and screen results for mesh session. An
// error reply carries no data; its code becomes the failure.
func terminalAgentData(response, request protocol.Control, host string) (any, bool, error) {
	if request.Type != protocol.TypeExecutionResize && request.Type != protocol.TypeExecutionScreen {
		return nil, false, nil
	}
	if response.Type == protocol.TypeError {
		return nil, true, nil
	}
	if request.Type == protocol.TypeExecutionResize {
		size, err := agentTerminalSize(response, request, host)
		return &size, true, err
	}
	screen, err := agentTerminalScreen(response, request, host)
	return &screen, true, err
}
