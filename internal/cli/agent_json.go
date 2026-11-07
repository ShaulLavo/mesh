package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/transport"
	"github.com/spf13/cobra"
)

type agentInvocation struct {
	operation string
	requestID string
	attempted bool
}

func protectAgentFlagErrors(command *cobra.Command, invocation *agentInvocation) {
	previous := command.FlagErrorFunc()
	command.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if invocation.operation != "" {
			return agentapi.Usage(invocation.operation, err.Error())
		}
		return previous(cmd, err)
	})
	for _, child := range command.Commands() {
		protectAgentFlagErrors(child, invocation)
	}
}

func (a *agentInvocation) write(output io.Writer, envelope agentapi.Envelope) error {
	a.attempted = true
	if a.requestID == "" {
		id, err := agentapi.NewRequestID()
		if err != nil {
			return fmt.Errorf("identify agent result: %w", err)
		}
		a.requestID = id
	}
	envelope.SchemaVersion, envelope.Operation, envelope.RequestID = agentapi.SchemaVersion, a.operation, a.requestID
	if err := agentapi.Write(output, envelope); err != nil {
		return fmt.Errorf("output agent result: %w", err)
	}
	if exit := envelope.ExitCode(); exit != 0 {
		return statusError{code: exit}
	}
	return nil
}

// WriteAgentFailure covers initialization failures before the command tree exists.
func WriteAgentFailure(output io.Writer, args []string, err error) error {
	invocation := agentInvocation{operation: agentapi.Classify(args)}
	return invocation.failure(output, err)
}

func (a *agentInvocation) failure(output io.Writer, err error) error {
	if err == nil || a.attempted {
		return err
	}
	failure := agentError(err, a.operation, "", agentapi.StateUnavailable)
	return a.write(output, agentapi.Envelope{Status: agentapi.StatusError, Errors: []agentapi.Error{failure}})
}

func agentError(err error, operation, target string, fallback agentapi.Code) agentapi.Error {
	var failure *agentapi.Failure
	if errors.As(err, &failure) {
		result := failure.Err
		result.Operation, result.Target = operation, target
		return result
	}
	code := fallback
	var rejected *ControlError
	var mismatch *hostIdentityError
	switch {
	case errors.As(err, &rejected):
		code = controlAgentCode(rejected.Code)
	case errors.As(err, &mismatch):
		code = agentapi.HostIdentityMismatch
	case errors.Is(err, transport.ErrAuthentication):
		code = agentapi.HostAuthenticationFailed
	}
	return agentapi.Fail(code, operation, target, err.Error()).Err
}

func controlAgentCode(code string) agentapi.Code {
	switch code {
	case "control.unknown":
		return agentapi.ProtocolUnsupported
	case "request.key_conflict", "request.in_progress", "request.outcome_unknown", "request.not_found", "resource.limit", "state.unavailable":
		return agentapi.Code(code)
	case "session.not_found":
		return agentapi.SessionNotFound
	default:
		return agentapi.InternalError
	}
}

// ControlError retains a daemon's code while preserving its human error text.
type ControlError struct {
	Code      string
	Operation string
	Message   string
	human     string
}

func (e *ControlError) Error() string { return e.human }

func typedControlError(code, operation, message, human string) error {
	return &ControlError{Code: code, Operation: operation, Message: safeRemoteText(message), human: human}
}

func (c *Command) ExecuteContext(ctx context.Context) error {
	err := c.Command.ExecuteContext(ctx)
	if c.agent.operation == "" {
		if err != nil {
			return fmt.Errorf("%w", err)
		}
		return nil
	}
	return c.agent.failure(c.OutOrStdout(), err)
}
