package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/worker"
)

type attachRequest struct {
	target  resolvedSession
	options AttachOptions
	quiet   bool
}

func (a *application) attach(cmd *cobra.Command, request attachRequest) error {
	options, display, err := a.prepareAttachment(cmd, request)
	if err != nil {
		return err
	}
	resolved := request.target
	restore := a.bindTerminal(bindingFor(resolved))
	a.noticeBeforeAttachment(cmd)
	result, err := Attach(cmd.Context(), options)
	if err != nil {
		if !result.Established {
			restore()
		}
		return err
	}
	output := cmd.ErrOrStderr()
	if request.quiet {
		output = io.Discard
	}
	return writeAttachResult(output, display, result)
}

func (a *application) prepareAttachment(cmd *cobra.Command, request attachRequest) (AttachOptions, string, error) {
	resolved, options := request.target, request.options
	if len(options.ContainingSessions) > 0 {
		target, err := resolvedTargetIdentity(resolved)
		if err != nil {
			return AttachOptions{}, "", fmt.Errorf("resolve attach target containment: %w", err)
		}
		if err := rejectContainingTarget(target, options.ContainingSessions); err != nil {
			return AttachOptions{}, "", err
		}
		options.HostID = target.HostID
	}
	var display string
	if resolved.local != nil {
		if resolved.local.Liveness == LivenessGone {
			return AttachOptions{}, "", stoppedSessionError(resolved.local.ID, "", resolved.local.State())
		}
		if options.SocketPath == "" {
			options.SocketPath = paths.Socket(resolved.local.Dir)
		}
		options.SessionID = resolved.local.ID
		display = resolved.local.ID
		return options, display, nil
	}
	if resolved.remote.State != worker.StateRunning && resolved.remote.State != worker.StateDetached {
		return AttachOptions{}, "", stoppedSessionError(resolved.remote.ID, HostLabel(*resolved.host), resolved.remote.State)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), wakeIntentTimeout)
	conn, err := openVerifiedHost(ctx, *resolved.host, a.intentDialer(cmd.ErrOrStderr()))
	cancel()
	if err != nil {
		return AttachOptions{}, "", err
	}
	options.Conn = conn
	options.HostID = resolved.host.ID
	options.SessionID = resolved.remote.ID
	display = resolved.remote.ID + " on " + HostLabel(*resolved.host)
	return options, display, nil
}

func writeAttachResult(output io.Writer, display string, result AttachResult) error {
	switch {
	case result.Exited:
		if _, err := fmt.Fprintf(output, "\r\nsession %s exited (%d)\r\n", display, result.ExitCode); err != nil {
			return fmt.Errorf("report session %s exit: %w", display, err)
		}
		if result.ExitCode != 0 {
			return statusError{code: result.ExitCode}
		}
		return nil
	case result.Detached:
		_, err := fmt.Fprintf(output, "\r\ndetached from %s, still running\r\n", display)
		if err != nil {
			return fmt.Errorf("report session %s detach: %w", display, err)
		}
		return nil
	default:
		_, err := fmt.Fprintf(output, "\r\ndisconnected from %s\r\n", display)
		if err != nil {
			return fmt.Errorf("report session %s disconnect: %w", display, err)
		}
		return nil
	}
}
