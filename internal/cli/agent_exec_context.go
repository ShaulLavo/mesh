package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/spf13/cobra"
)

type contextOptions struct {
	host, identity, key, cwd, shell, label string
	env, setEnv, unsetEnv                  []string
	expectedRevision                       uint64
	json                                   bool
	timeout                                time.Duration
}

func (a *application) contextCommand() *cobra.Command {
	group := &cobra.Command{Use: "context", Short: "Open and manage durable working contexts for an agent", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	}}
	group.AddCommand(a.contextOpenCommand(), a.contextGetCommand(), a.contextListCommand(), a.contextUpdateCommand(), a.contextCloseCommand(), a.contextReceiptCommand())
	return group
}

func contextCommonFlags(cmd *cobra.Command, o *contextOptions) {
	cmd.Flags().StringVar(&o.host, "host", "", "online machine name or exact host ID; implied by a context handle")
	cmd.Flags().StringVar(&o.identity, "identity", "", "local application identity, required")
	cmd.Flags().BoolVar(&o.json, "json", false, "write one agent result, required")
	cmd.Flags().DurationVar(&o.timeout, "timeout", 10*time.Second, "hard deadline from 100ms to 60s")
}

func (a *application) contextOpenCommand() *cobra.Command {
	o := &contextOptions{}
	cmd := &cobra.Command{Use: "open", Short: "Open a durable working context on a host", Args: profileArgs("contexts.open"), RunE: func(cmd *cobra.Command, _ []string) error {
		env, err := parseEnvAssignments(o.env)
		if err != nil {
			return agentapi.Usage("contexts.open", err.Error())
		}
		spec := &protocol.ContextSpec{Cwd: o.cwd, Shell: o.shell, Env: env, Label: o.label}
		return a.runContextControl(cmd, "contexts.open", *o, "", protocol.Control{Type: protocol.TypeContextOpen, RequestKey: o.key, ExecutionContext: &protocol.ExecutionContextRequest{Open: spec}})
	}}
	contextCommonFlags(cmd, o)
	cmd.Flags().StringVar(&o.key, "request-key", "", "saved logical request key, required")
	cmd.Flags().StringVar(&o.cwd, "cwd", "", "absolute directory on the target, required")
	cmd.Flags().StringVar(&o.shell, "shell", "", "shell for command strings: login (default), sh or none")
	cmd.Flags().StringArrayVar(&o.env, "env", nil, "overlay variable NAME=VALUE, repeatable")
	cmd.Flags().StringVar(&o.label, "label", "", "display label")
	return cmd
}

func (a *application) contextGetCommand() *cobra.Command {
	return a.contextTargetCommand("get", "contexts.get", protocol.TypeContextGet, "Read one context")
}

func (a *application) contextCloseCommand() *cobra.Command {
	cmd := a.contextTargetCommand("close", "contexts.close", protocol.TypeContextClose, "Stop new jobs in a context; running jobs keep going")
	return cmd
}

func (a *application) contextTargetCommand(name, operation, kind, short string) *cobra.Command {
	o := &contextOptions{}
	cmd := &cobra.Command{Use: name + " CONTEXT", Short: short, Args: exactContextArgs(operation), RunE: func(cmd *cobra.Command, args []string) error {
		params := &protocol.ExecutionContextRequest{}
		if kind == protocol.TypeContextClose && cmd.Flags().Changed("expected-revision") {
			params.ExpectedRevision = &o.expectedRevision
		}
		return a.runContextControl(cmd, operation, *o, args[0], protocol.Control{Type: kind, ExecutionContext: params})
	}}
	contextCommonFlags(cmd, o)
	if kind == protocol.TypeContextClose {
		cmd.Flags().Uint64Var(&o.expectedRevision, "expected-revision", 0, "fail if the context changed since this revision")
	}
	return cmd
}

func (a *application) contextListCommand() *cobra.Command {
	o := &contextOptions{}
	cmd := &cobra.Command{Use: "ls", Aliases: []string{agentSessionListCommand}, Short: "List the application's contexts on a host", Args: profileArgs("contexts.list"), RunE: func(cmd *cobra.Command, _ []string) error {
		return a.runContextControl(cmd, "contexts.list", *o, "", protocol.Control{Type: protocol.TypeContextList})
	}}
	contextCommonFlags(cmd, o)
	return cmd
}

func (a *application) contextUpdateCommand() *cobra.Command {
	o := &contextOptions{}
	cmd := &cobra.Command{Use: "update CONTEXT", Short: "Change a context at an expected revision", Args: exactContextArgs("contexts.update"), RunE: func(cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("expected-revision") {
			return agentapi.Usage("contexts.update", "Pass --expected-revision N.")
		}
		set, err := parseEnvAssignments(o.setEnv)
		if err != nil {
			return agentapi.Usage("contexts.update", err.Error())
		}
		change := &protocol.ContextChange{Cwd: o.cwd, SetEnv: set, UnsetEnv: o.unsetEnv}
		if cmd.Flags().Changed("label") {
			change.Label = &o.label
		}
		params := &protocol.ExecutionContextRequest{ExpectedRevision: &o.expectedRevision, Change: change}
		return a.runContextControl(cmd, "contexts.update", *o, args[0], protocol.Control{Type: protocol.TypeContextUpdate, ExecutionContext: params})
	}}
	contextCommonFlags(cmd, o)
	cmd.Flags().Uint64Var(&o.expectedRevision, "expected-revision", 0, "revision before this change, required")
	cmd.Flags().StringVar(&o.cwd, "cwd", "", "new absolute directory")
	cmd.Flags().StringArrayVar(&o.setEnv, "set-env", nil, "set overlay variable NAME=VALUE, repeatable")
	cmd.Flags().StringArrayVar(&o.unsetEnv, "unset-env", nil, "remove overlay variable NAME, repeatable")
	cmd.Flags().StringVar(&o.label, "label", "", "new display label")
	return cmd
}

func (a *application) contextReceiptCommand() *cobra.Command {
	o := &contextOptions{}
	cmd := &cobra.Command{Use: "receipt", Short: "Look up a context by the key that opened it", Args: profileArgs("contexts.receipt"), RunE: func(cmd *cobra.Command, _ []string) error {
		return a.runContextControl(cmd, "contexts.receipt", *o, "", protocol.Control{Type: protocol.TypeContextReceipt, RequestKey: o.key})
	}}
	contextCommonFlags(cmd, o)
	cmd.Flags().StringVar(&o.key, "request-key", "", "request key used to open, required")
	return cmd
}

func exactContextArgs(operation string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return agentapi.Usage(operation, "Pass one context handle or context ID.")
		}
		return nil
	}
}

// contextHostValue picks the host to dial: --host, or the host a handle names.
func contextHostValue(operation, host, target string) (string, error) {
	if host != "" || target == "" || protocol.ValidContextID(target) {
		return host, nil
	}
	handleHost, _, err := protocol.ParseContextHandle(target)
	if err != nil {
		return "", agentapi.Usage(operation, err.Error())
	}
	return handleHost, nil
}

func parseEnvAssignments(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(values))
	for _, value := range values {
		name, val, ok := strings.Cut(value, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("environment %q must be NAME=VALUE", value)
		}
		env[name] = val
	}
	return env, nil
}

func (o contextOptions) validate(operation string) error {
	if !o.json || strings.TrimSpace(o.identity) == "" {
		return agentapi.Usage(operation, "Pass --identity NAME and --json.")
	}
	if o.timeout < 100*time.Millisecond || o.timeout > 60*time.Second {
		return agentapi.Usage(operation, "--timeout must be from 100ms to 60s.")
	}
	if o.key != "" || operation == "contexts.open" || operation == "contexts.receipt" {
		key := execution.Key{Host: "validation", Class: execution.ApplicationClass, Principal: "caller", Opaque: o.key, Operation: execution.OperationContextOpen}
		if err := key.Validate(); err != nil {
			return agentapi.Usage(operation, err.Error())
		}
	}
	return nil
}

func (a *application) runContextControl(cmd *cobra.Command, operation string, o contextOptions, target string, request protocol.Control) error {
	if err := o.validate(operation); err != nil {
		return err
	}
	hostValue, err := contextHostValue(operation, o.host, target)
	if err != nil {
		return err
	}
	if strings.TrimSpace(hostValue) == "" {
		return agentapi.Usage(operation, "Pass --host HOST or a context handle.")
	}
	identity, err := loadApplicationIdentity(operation, o.identity)
	if err != nil {
		return err
	}
	request.Caller = protocol.CallerCredential(identity.Credential.Encode())
	ctx, cancel := context.WithTimeout(cmd.Context(), o.timeout)
	defer cancel()
	host, err := a.agentSessionTarget(ctx, hostValue)
	if err != nil {
		return err
	}
	if target != "" {
		id, err := contextOnHost(operation, host.ID, target)
		if err != nil {
			return err
		}
		request.ExecutionContext.ContextID = id
	}
	if request.Type == protocol.TypeContextList {
		return a.listAllContexts(ctx, cmd, host, request)
	}
	response, err := a.exchangeAgentControl(ctx, host, request)
	if err != nil {
		return err
	}
	return a.writeContextReply(cmd, request, response, host.ID)
}

// listAllContexts follows list pages; a host holds at most 4,096 contexts.
func (a *application) listAllContexts(ctx context.Context, cmd *cobra.Command, host HostRecord, request protocol.Control) error {
	merged := protocol.Control{Type: protocol.TypeContextResult, ContextInfos: []protocol.ExecutionContextInfo{}}
	request.ExecutionContext = &protocol.ExecutionContextRequest{}
	for range maxContextListPages {
		response, err := a.exchangeAgentControl(ctx, host, request)
		if err != nil {
			return err
		}
		if response.Type != protocol.TypeContextResult {
			return a.writeContextReply(cmd, request, response, host.ID)
		}
		merged.ContextInfos = append(merged.ContextInfos, response.ContextInfos...)
		if response.ContextNextCursor == "" {
			return a.writeContextReply(cmd, request, merged, host.ID)
		}
		request.ExecutionContext.Cursor = response.ContextNextCursor
	}
	return fmt.Errorf("host returned more context pages than it can hold")
}

const maxContextListPages = 4096

func (a *application) exchangeAgentControl(ctx context.Context, host HostRecord, request protocol.Control) (protocol.Control, error) {
	conn, _, err := openAgentHostInfo(ctx, host, a.dependencies.DialPassive)
	if err != nil {
		return protocol.Control{}, agentapi.Fail(agentError(err, a.agent.operation, host.ID, agentapi.HostUnreachable).Code, a.agent.operation, host.ID, err.Error())
	}
	defer func() { _ = conn.Close() }()
	request.RequestID = a.agent.requestID
	if request.RequestID == "" {
		request.RequestID, err = newDaemonRequestID()
		if err != nil {
			return protocol.Control{}, err
		}
	}
	return controlRequest(ctx, conn, request)
}

func (a *application) writeContextReply(cmd *cobra.Command, request, response protocol.Control, host string) error {
	var data any
	if response.ContextInfo != nil {
		data = agentContext(*response.ContextInfo)
	}
	if request.Type == protocol.TypeContextList && response.Type == protocol.TypeContextResult {
		list := agentapi.ExecutionContexts{HostID: host, Contexts: make([]agentapi.ExecutionContext, 0, len(response.ContextInfos))}
		for _, info := range response.ContextInfos {
			list.Contexts = append(list.Contexts, agentContext(info))
		}
		data = list
	}
	if response.Type == protocol.TypeError {
		failure := agentapi.Fail(controlAgentCode(response.ErrorCode), a.agent.operation, host, response.Message).Err
		if data == nil {
			return &agentapi.Failure{Err: failure}
		}
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusPartial, Data: data, Errors: []agentapi.Error{failure}})
	}
	if response.Type != protocol.TypeContextResult || data == nil {
		return fmt.Errorf("host did not return a context result")
	}
	return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: data})
}

func agentContext(info protocol.ExecutionContextInfo) agentapi.ExecutionContext {
	names := info.EnvNames
	if names == nil {
		names = []string{}
	}
	return agentapi.ExecutionContext{HostID: info.HostID, ContextID: info.ContextID, Handle: info.Handle, RequestKey: info.RequestKey, State: info.State, Revision: info.Revision, Cwd: info.Cwd, Shell: info.Shell, ShellPath: info.ShellPath, Label: info.Label, EnvNames: names, EnvDigest: info.EnvDigest, RunningJobs: info.RunningJobs, UncertainJobs: info.UncertainJobs, CreatedAt: info.CreatedAt, LastUsedAt: info.LastUsedAt, ClosedAt: info.ClosedAt, ExpiresAt: info.ExpiresAt}
}
