package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTailscaleServeTimeout = 15 * time.Second
	maximumCommandOutput         = 64 << 10
	externalCommandWaitDelay     = time.Second
)

type externalCommand func(context.Context, string, ...string) ([]byte, error)
type serveForwardVerifier func(context.Context, uint16) error

func configureTailscaleServe(ctx context.Context, httpsPort uint16, timeout time.Duration, run externalCommand) error {
	if ctx == nil {
		return errors.New("daemon: configure Tailscale Serve with nil context")
	}
	if httpsPort == 0 {
		return errors.New("daemon: configure Tailscale Serve with zero HTTPS port")
	}
	if timeout <= 0 {
		return errors.New("daemon: configure Tailscale Serve with non-positive timeout")
	}
	if run == nil {
		return errors.New("daemon: configure Tailscale Serve with nil command runner")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("daemon: configure Tailscale Serve: %w", err)
	}

	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := run(commandCtx, "tailscale", tailscaleServeArguments(httpsPort)...)
	if err == nil {
		return nil
	}
	if commandCtx.Err() != nil {
		return fmt.Errorf("daemon: configure Tailscale Serve: %w", commandCtx.Err())
	}
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("daemon: configure Tailscale Serve: %w", err)
	}
	return fmt.Errorf("daemon: configure Tailscale Serve: %w: %s", err, detail)
}

func verifyTailscaleServeForward(ctx context.Context, httpsPort uint16, timeout time.Duration, verify serveForwardVerifier) error {
	if ctx == nil {
		return errors.New("daemon: verify Tailscale Serve with nil context")
	}
	if httpsPort == 0 {
		return errors.New("daemon: verify Tailscale Serve with zero HTTPS port")
	}
	if timeout <= 0 {
		return errors.New("daemon: verify Tailscale Serve with non-positive timeout")
	}
	if verify == nil {
		return errors.New("daemon: verify Tailscale Serve with nil verifier")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("daemon: verify Tailscale Serve: %w", err)
	}

	verifyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := verify(verifyCtx, httpsPort); err != nil {
		if verifyCtx.Err() != nil {
			return fmt.Errorf("daemon: verify Tailscale Serve: %w", verifyCtx.Err())
		}
		return fmt.Errorf("daemon: verify Tailscale Serve: %w; expected persistent setup: %q", err, tailscaleServeCommand(httpsPort))
	}
	return nil
}

func tailscaleServeArguments(httpsPort uint16) []string {
	target := "tcp://127.0.0.1:" + strconv.Itoa(int(httpsPort))
	return []string{"serve", "--bg", "--yes", "--tcp=443", target}
}

func tailscaleServeCommand(httpsPort uint16) string {
	return "tailscale " + strings.Join(tailscaleServeArguments(httpsPort), " ")
}

func runExternalCommand(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...) //nolint:gosec // production fixes the command to Tailscale; tests inject bounded cancellation fixtures
	output := &boundedCommandOutput{}
	command.Stdout = output
	command.Stderr = output
	command.WaitDelay = externalCommandWaitDelay
	err := command.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	contents := append([]byte(nil), output.Bytes()...)
	if output.truncated {
		contents = append(contents, []byte("\n[output truncated]")...)
	}
	return contents, err
}

type boundedCommandOutput struct {
	bytes.Buffer
	truncated bool
}

func (w *boundedCommandOutput) Write(value []byte) (int, error) {
	written := len(value)
	remaining := maximumCommandOutput - w.Len()
	if remaining <= 0 {
		w.truncated = true
		return written, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		w.truncated = true
	}
	_, err := w.Buffer.Write(value)
	return written, err
}
