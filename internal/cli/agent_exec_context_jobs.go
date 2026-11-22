package cli

import (
	"fmt"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/spf13/cobra"
)

// attachApplicationCaller acts as the named identity, or marks a raw
// control on an exact instance as explicit whole-account administration.
func attachApplicationCaller(operation, hostID string, o agentExecutionOptions, request *protocol.Control) error {
	if o.identity == "" {
		if o.context != "" {
			return agentapi.Usage(operation, "--context needs --identity.")
		}
		request.Administration = request.Instance != ""
		return nil
	}
	if o.context != "" {
		id, err := contextOnHost(operation, hostID, o.context)
		if err != nil {
			return err
		}
		if request.ExecutionContext == nil {
			request.ExecutionContext = &protocol.ExecutionContextRequest{}
		}
		request.ExecutionContext.ContextID = id
	}
	identity, err := loadApplicationIdentity(operation, o.identity)
	if err != nil {
		return err
	}
	request.Caller = protocol.CallerCredential(identity.Credential.Encode())
	return nil
}

// contextOnHost returns the context ID of a bare ID or handle, refusing a
// handle that names another host than the one the command reached.
func contextOnHost(operation, hostID, target string) (string, error) {
	if protocol.ValidContextID(target) {
		return target, nil
	}
	handleHost, id, err := protocol.ParseContextHandle(target)
	if err != nil {
		return "", agentapi.Usage(operation, err.Error())
	}
	if handleHost != hostID {
		return "", agentapi.Fail(agentapi.HostIdentityMismatch, operation, hostID, fmt.Sprintf("context handle names host %s, not %s", handleHost, hostID))
	}
	return id, nil
}

// runContextJobCreate starts a pipe job in a context. The host resolves its
// directory, shell and environment against the context's current revision.
func (a *application) runContextJobCreate(cmd *cobra.Command, o agentExecutionOptions, args []string) error {
	const operation = "sessions.create"
	if o.context == "" {
		return agentapi.Usage(operation, "--identity jobs need --context.")
	}
	if (len(args) == 0) == (o.shellCommand == "") {
		return agentapi.Usage(operation, "Pass either -- argv or --shell-command.")
	}
	if len(args) > 0 && cmd.ArgsLenAtDash() != 0 {
		return agentapi.Usage(operation, "Put command arguments after --.")
	}
	request := protocol.Control{Type: protocol.TypeExecutionCreate, ExecutionKind: o.kind, RequestKey: o.key, Command: args, Cwd: o.cwd, Stdin: o.stdin, ExecutionContext: &protocol.ExecutionContextRequest{ShellCommand: o.shellCommand}}
	if err := contextJobTerminal(cmd, o, &request); err != nil {
		return agentapi.Usage(operation, err.Error())
	}
	return a.runAgentExecution(cmd, o, request)
}

// contextJobTerminal applies terminal settings to a context job. A terminal
// job takes input through its terminal, so it has no stdin mode.
func contextJobTerminal(cmd *cobra.Command, o agentExecutionOptions, request *protocol.Control) error {
	switch o.kind {
	case execution.KindPipe:
		if cmd.Flags().Changed("cols") || cmd.Flags().Changed("rows") || cmd.Flags().Changed("term") {
			return fmt.Errorf("terminal settings require --kind pty")
		}
		return nil
	case execution.KindPTY:
		if o.stdin != "" {
			return fmt.Errorf("terminal jobs take input through their terminal; omit --stdin")
		}
		request.Cols, request.Rows, request.Term = o.cols, o.rows, o.term
		return nil
	default:
		return fmt.Errorf("choose --kind pty or --kind pipe")
	}
}
