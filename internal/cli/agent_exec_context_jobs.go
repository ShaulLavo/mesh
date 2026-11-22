package cli

import (
	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/spf13/cobra"
)

// attachApplicationCaller acts as the named identity, or marks a raw
// control on an exact instance as explicit whole-account administration.
func attachApplicationCaller(operation string, o agentExecutionOptions, request *protocol.Control) error {
	if o.identity == "" {
		if o.context != "" {
			return agentapi.Usage(operation, "--context needs --identity.")
		}
		request.Administration = request.Instance != ""
		return nil
	}
	identity, err := loadApplicationIdentity(operation, o.identity)
	if err != nil {
		return err
	}
	request.Caller = protocol.CallerCredential(identity.Credential.Encode())
	if o.context == "" {
		return nil
	}
	_, id, err := resolveContextTarget(operation, "", o.context)
	if err != nil {
		return err
	}
	if request.ExecutionContext == nil {
		request.ExecutionContext = &protocol.ExecutionContextRequest{}
	}
	request.ExecutionContext.ContextID = id
	return nil
}

// runContextJobCreate starts a pipe job in a context. The host resolves its
// directory, shell and environment against the context's current revision.
func (a *application) runContextJobCreate(cmd *cobra.Command, o agentExecutionOptions, args []string) error {
	const operation = "sessions.create"
	if o.context == "" {
		return agentapi.Usage(operation, "--identity jobs need --context.")
	}
	if o.kind != execution.KindPipe {
		return agentapi.Usage(operation, "Context jobs use --kind pipe.")
	}
	if (len(args) == 0) == (o.shellCommand == "") {
		return agentapi.Usage(operation, "Pass either -- argv or --shell-command.")
	}
	if len(args) > 0 && cmd.ArgsLenAtDash() != 0 {
		return agentapi.Usage(operation, "Put command arguments after --.")
	}
	request := protocol.Control{Type: protocol.TypeExecutionCreate, ExecutionKind: o.kind, RequestKey: o.key, Command: args, Cwd: o.cwd, Stdin: o.stdin, ExecutionContext: &protocol.ExecutionContextRequest{ShellCommand: o.shellCommand}}
	return a.runAgentExecution(cmd, o, request)
}
