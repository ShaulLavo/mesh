package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/spf13/cobra"
)

type agentExecutionOptions struct {
	host, key, kind, cwd, term string
	json                       bool
	timeout                    time.Duration
	cols, rows                 int
}

func (a *application) sessionCreateCommand() *cobra.Command {
	o := &agentExecutionOptions{}
	command := &cobra.Command{Use: "create -- command [args...]", Short: "Start a keyed detached PTY command on an online host", Args: cobra.ArbitraryArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if err := o.validate("sessions.create"); err != nil {
			return err
		}
		if cmd.ArgsLenAtDash() != 0 {
			return agentapi.Usage("sessions.create", "Put command arguments after --.")
		}
		intent, err := execution.Normalize(execution.Intent{Kind: o.kind, Command: args, Cwd: o.cwd, Cols: o.cols, Rows: o.rows, Term: o.term})
		if err != nil {
			return agentapi.Usage("sessions.create", err.Error())
		}
		return a.runAgentExecution(cmd, *o, protocol.Control{Type: protocol.TypeExecutionCreate, ExecutionKind: intent.Kind, RequestKey: o.key, Command: intent.Command, Cwd: intent.Cwd, Cols: intent.Cols, Rows: intent.Rows, Term: intent.Term})
	}}
	agentExecutionFlags(command, o, 40*time.Second)
	command.Flags().StringVar(&o.kind, "kind", "", "explicit session kind, currently pty")
	command.Flags().StringVar(&o.cwd, "cwd", "", "absolute clean working directory on the target")
	command.Flags().StringVar(&o.term, "term", "dumb", "target terminal type")
	command.Flags().IntVar(&o.cols, "cols", 80, "initial terminal width")
	command.Flags().IntVar(&o.rows, "rows", 24, "initial terminal height")
	return command
}
func (a *application) sessionRequestCommand() *cobra.Command {
	o := &agentExecutionOptions{}
	command := &cobra.Command{Use: "request", Short: "Look up a retained creation receipt without launching work", Args: profileArgs("sessions.request"), RunE: func(cmd *cobra.Command, _ []string) error {
		if err := o.validate("sessions.request"); err != nil {
			return err
		}
		return a.runAgentExecution(cmd, *o, protocol.Control{Type: protocol.TypeExecutionRequest, RequestKey: o.key})
	}}
	agentExecutionFlags(command, o, 5*time.Second)
	return command
}
func agentExecutionFlags(cmd *cobra.Command, o *agentExecutionOptions, budget time.Duration) {
	cmd.Flags().StringVar(&o.host, "host", "", "machine name or exact host ID, required")
	cmd.Flags().StringVar(&o.key, "request-key", "", "saved logical request key, required")
	cmd.Flags().BoolVar(&o.json, "json", false, "write one agent result, required")
	cmd.Flags().DurationVar(&o.timeout, "timeout", budget, "hard deadline from 100ms to 60s")
}
func (o agentExecutionOptions) validate(operation string) error {
	if !o.json || strings.TrimSpace(o.host) == "" {
		return agentapi.Usage(operation, "Pass --host HOST and --json.")
	}
	if o.timeout < 100*time.Millisecond || o.timeout > 60*time.Second {
		return agentapi.Usage(operation, "--timeout must be from 100ms to 60s.")
	}
	if err := (execution.Key{Host: "validation", Class: "local-account", Principal: "account", Opaque: o.key}).Validate(); err != nil {
		return agentapi.Usage(operation, err.Error())
	}
	return nil
}
func (a *application) runAgentExecution(cmd *cobra.Command, o agentExecutionOptions, request protocol.Control) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), o.timeout)
	defer cancel()
	host, err := a.agentSessionTarget(ctx, o.host)
	if err != nil {
		return err
	}
	conn, _, err := openAgentHostInfo(ctx, host, a.dependencies.DialPassive)
	if err != nil {
		return agentapi.Fail(agentError(err, a.agent.operation, host.ID, agentapi.HostUnreachable).Code, a.agent.operation, host.ID, err.Error())
	}
	defer func() { _ = conn.Close() }()
	request.RequestID = a.agent.requestID
	if request.RequestID == "" {
		request.RequestID, err = newDaemonRequestID()
		if err != nil {
			return err
		}
	}
	if request.Type == protocol.TypeExecutionWait {
		deadline, _ := ctx.Deadline()
		request.TimeoutMillis = max(1, int(time.Until(deadline)/time.Millisecond)-20)
	}
	response, err := controlRequest(ctx, conn, request)
	if err != nil {
		return err
	}
	data, err := executionAgentData(response, request, host.ID, o.key)
	if err != nil {
		return err
	}
	if response.Type == protocol.TypeError {
		failure := agentapi.Fail(controlAgentCode(response.ErrorCode), a.agent.operation, host.ID, response.Message).Err
		if data == nil {
			return &agentapi.Failure{Err: failure}
		}
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusPartial, Data: data, Errors: []agentapi.Error{failure}})
	}
	if response.Type != protocol.TypeExecutionReceipt || data == nil {
		return fmt.Errorf("host did not return an execution receipt")
	}
	return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: data})
}
func agentExecutionReceipt(r *protocol.ExecutionReceipt, host, key string) (*agentapi.ExecutionReceipt, error) {
	id, idErr := session.ParseID(r.SessionID)
	if idErr != nil || id != r.SessionID {
		return nil, fmt.Errorf("host returned an invalid session ID")
	}
	if err := validateAgentExecutionState(r); err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(r.Instance)
	if err != nil || len(raw) != 16 || hex.EncodeToString(raw) != r.Instance || r.HostID != host || (key != "" && r.RequestKey != key) || r.Kind != executionPTYKind {
		return nil, fmt.Errorf("host returned an invalid execution receipt")
	}
	out := &agentapi.ExecutionReceipt{HostID: r.HostID, SessionID: r.SessionID, Instance: r.Instance, RequestKey: r.RequestKey, KeyScope: r.KeyScope, Kind: r.Kind, Phase: r.Phase, Acceptance: r.Acceptance, LaunchOutcome: r.LaunchOutcome, Publication: r.Publication, Removal: r.Removal, ReceiptRetainUntil: r.ReceiptRetainUntil, RetentionReason: r.RetentionReason, OutputRetainUntil: r.OutputRetainUntil, OutputExpired: r.OutputExpired, KillRequested: r.KillRequested}
	if r.Result != nil {
		t := r.Result.Termination
		out.Result = &agentapi.ExecutionResult{Termination: agentapi.ExecutionTermination{Kind: t.Kind, Code: t.Code, Signal: t.Signal, Reason: t.Reason}, EndedAt: r.Result.EndedAt, Capture: r.Result.Capture, Streams: agentExecutionStreams(r.Result.Streams)}
	}
	return out, nil
}

func executionBoundaryError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func validateAgentExecutionState(r *protocol.ExecutionReceipt) error {
	if r.KeyScope != executionDeviceScope && r.KeyScope != "local-account" {
		return fmt.Errorf("host returned an invalid key scope")
	}
	switch r.Acceptance {
	case "before_publish", "visible_unconfirmed", "durable":
	default:
		return fmt.Errorf("host returned an invalid acceptance")
	}
	switch r.Phase {
	case "reserved", "dispatched", executionLivePhase, "uncertain":
		if r.Result != nil || r.ReceiptRetainUntil != nil {
			return fmt.Errorf("host returned contradictory active receipt")
		}
	case "terminal":
		if r.Result == nil {
			return fmt.Errorf("host omitted its terminal result")
		}
	default:
		return fmt.Errorf("host returned an invalid execution phase")
	}
	return validateAgentExecutionResult(r.Result)
}
func validateAgentExecutionResult(result *protocol.ExecutionResult) error {

	if result == nil {
		return nil
	}
	if result.Capture != "not_recorded" && result.Capture != "complete" && result.Capture != "incomplete" {
		return fmt.Errorf("host returned unsupported capture")
	}
	t := result.Termination
	switch t.Kind {
	case "exit":
		if t.Code == nil || *t.Code < 0 || t.Signal != nil {
			return fmt.Errorf("host returned an invalid exit")
		}
	case "killed":
		if (t.Signal == nil) == (t.Code == nil) {
			return fmt.Errorf("host returned an invalid killed result")
		}
	case "signal":
		if t.Signal == nil || *t.Signal <= 0 || t.Code != nil {
			return fmt.Errorf("host returned an invalid signal")
		}
	case "not_started", "interrupted":
		if t.Code != nil || t.Signal != nil || t.Reason == "" {
			return fmt.Errorf("host returned an invalid non-exit")
		}
	default:
		return fmt.Errorf("host returned invalid termination")
	}
	return nil
}

const executionDeviceScope = "device"
const executionLivePhase = "live"
