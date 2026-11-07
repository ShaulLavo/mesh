package cli

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/worker"
	"github.com/spf13/cobra"
)

type executionControlOptions struct {
	agentExecutionOptions
	instance, stream, cursor, signal string
	maxBytes                         int
}

func (a *application) sessionWaitCommand() *cobra.Command {
	return a.sessionExecutionCommand("wait", protocol.TypeExecutionWait)
}
func (a *application) sessionLogsCommand() *cobra.Command {
	return a.sessionExecutionCommand("logs", protocol.TypeExecutionLogs)
}
func (a *application) sessionSignalCommand() *cobra.Command {
	return a.sessionExecutionCommand("signal", protocol.TypeExecutionSignal)
}
func (a *application) sessionKillCommand() *cobra.Command {
	return a.sessionExecutionCommand("kill", protocol.TypeExecutionKill)
}
func (a *application) sessionRemoveCommand() *cobra.Command {
	return a.sessionExecutionCommand("rm", protocol.TypeExecutionRemove)
}
func (a *application) sessionExecutionCommand(name, kind string) *cobra.Command {
	o := &executionControlOptions{}
	operation := "sessions." + name
	if name == "rm" {
		operation = "sessions.remove"
	}
	command := &cobra.Command{Use: name + " SESSION_ID", Short: executionControlSummary(kind), Args: exactExecutionArgs(operation), RunE: func(cmd *cobra.Command, args []string) error {
		if err := o.validateControl(operation, args[0], kind); err != nil {
			return err
		}
		request := protocol.Control{Type: kind, SessionID: args[0], Instance: o.instance, Stream: o.stream, Cursor: o.cursor, MaxBytes: o.maxBytes, Signal: o.signal}
		if kind == protocol.TypeExecutionWait {
			request.TimeoutMillis = int(o.timeout / time.Millisecond)
		}
		return a.runAgentExecution(cmd, o.agentExecutionOptions, request)
	}}
	command.Flags().StringVar(&o.host, "host", "", "online machine name or exact host ID, required")
	command.Flags().BoolVar(&o.json, "json", false, "write one agent result, required")
	command.Flags().DurationVar(&o.timeout, "timeout", 5*time.Second, "finite deadline from 100ms to 60s")
	command.Flags().StringVar(&o.instance, "instance", "", "exact instance from the creation receipt, required")
	if kind == protocol.TypeExecutionLogs {
		command.Flags().StringVar(&o.stream, "stream", executionPTYKind, "exact stream, currently pty")
		command.Flags().StringVar(&o.cursor, "cursor", "", "saved independent output cursor")
		command.Flags().IntVar(&o.maxBytes, "max-bytes", 65536, "decoded page size from 1 to 1048576 bytes")
	}
	if kind == protocol.TypeExecutionSignal {
		command.Flags().StringVar(&o.signal, "signal", "", "process group signal, required")
	}
	return command
}
func executionControlSummary(kind string) string {
	switch kind {
	case protocol.TypeExecutionWait:
		return "Wait for a durable result without stopping the command"
	case protocol.TypeExecutionLogs:
		return "Read exact bounded PTY bytes with an independent cursor"
	case protocol.TypeExecutionSignal:
		return "Signal the exact keyed command instance"
	case protocol.TypeExecutionKill:
		return "Stop the exact keyed command instance"
	default:
		return "Remove a finished keyed command payload while retaining its receipt"
	}
}
func (o executionControlOptions) validateControl(operation, id, kind string) error {
	if !o.json || strings.TrimSpace(o.host) == "" {
		return agentapi.Usage(operation, "Pass --host HOST and --json.")
	}
	if o.timeout < 100*time.Millisecond || o.timeout > 60*time.Second {
		return agentapi.Usage(operation, "--timeout must be from 100ms to 60s.")
	}
	parsed, err := session.ParseID(id)
	token, tokenErr := hex.DecodeString(o.instance)
	if err != nil || parsed != id || tokenErr != nil || len(token) != 16 || hex.EncodeToString(token) != o.instance {
		return agentapi.Usage(operation, "Pass a canonical session ID and its exact --instance.")
	}
	if kind == protocol.TypeExecutionSignal && !worker.SupportsSignal(o.signal) {
		return agentapi.Usage(operation, "Pass a supported --signal.")
	}
	if kind == protocol.TypeExecutionLogs && (o.stream != executionPTYKind || o.maxBytes < 1 || o.maxBytes > 1<<20 || len(o.cursor) > 2048) {
		return agentapi.Usage(operation, "Use --stream pty and --max-bytes from 1 to 1048576.")
	}
	return nil
}
func agentExecutionStreams(streams []protocol.ExecutionStream) []agentapi.ExecutionStream {
	out := make([]agentapi.ExecutionStream, 0, len(streams))
	for _, s := range streams {
		out = append(out, agentapi.ExecutionStream{Stream: s.Stream, Generation: s.Generation, Earliest: s.Earliest, End: s.End, ObservedEnd: s.ObservedEnd, EOF: s.EOF, CloseReason: s.CloseReason, Loss: s.Loss})
	}
	return out
}
func agentExecutionPage(p *protocol.ExecutionPage, host string, request protocol.Control) (*agentapi.ExecutionPage, error) {
	if p.HostID != host || p.SessionID != request.SessionID || p.Instance != request.Instance || p.Stream != executionPTYKind || len(p.Bytes) > request.MaxBytes {
		return nil, fmt.Errorf("host returned an invalid exact page")
	}
	return &agentapi.ExecutionPage{HostID: p.HostID, SessionID: p.SessionID, Instance: p.Instance, Stream: p.Stream, Generation: p.Generation, Bytes: p.Bytes, StartCursor: p.StartCursor, NextCursor: p.NextCursor, EarliestCursor: p.EarliestCursor, EndCursor: p.EndCursor, ObservedEnd: p.ObservedEnd, StreamClosed: p.StreamClosed, EOF: p.EOF, CloseReason: p.CloseReason, Loss: p.Loss, Available: p.Available, Gap: p.Gap, Reason: p.Reason}, nil
}
func executionAgentData(response, request protocol.Control, host, key string) (any, error) {
	if response.ExecutionPage != nil {
		return agentExecutionPage(response.ExecutionPage, host, request)
	}
	if response.ExecutionReceipt == nil {
		return nil, nil
	}
	receipt, err := agentExecutionReceipt(response.ExecutionReceipt, host, key)
	if err != nil {
		return nil, err
	}
	if request.Instance != "" && (receipt.Instance != request.Instance || receipt.SessionID != request.SessionID) {
		return nil, fmt.Errorf("host returned another execution instance")
	}
	if request.Type == protocol.TypeExecutionWait || request.Type == protocol.TypeExecutionSignal || request.Type == protocol.TypeExecutionKill {
		return &agentapi.ExecutionObservation{Receipt: receipt, Wait: response.ExecutionWait, Delivery: response.ExecutionDelivery}, nil
	}
	return receipt, nil
}

func exactExecutionArgs(operation string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return agentapi.Usage(operation, "Pass one exact session ID.")
		}
		return nil
	}
}

const executionPTYKind = "pty"
