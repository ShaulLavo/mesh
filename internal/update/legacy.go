package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func IsLegacyError(err error) bool {
	var remote *RemoteError
	return errors.As(err, &remote) && strings.Contains(remote.Problem, `unknown control "update.control"`)
}

func Exchange(ctx context.Context, host Host, request protocol.Control) (protocol.Control, error) {
	if err := host.Validate(); err != nil {
		return protocol.Control{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := dial(ctx, host)
	if err != nil {
		return protocol.Control{}, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	payload, err := request.Encode()
	if err != nil {
		return protocol.Control{}, err
	}
	if err = conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return protocol.Control{}, err
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return protocol.Control{}, err
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return response, err
	}
	if response.Type == protocol.TypeError {
		return response, &RemoteError{Problem: response.Message}
	}
	if frame.Kind != protocol.KindControl || response.RequestID != request.RequestID {
		return response, errors.New("legacy update response request mismatch")
	}
	return response, nil
}

func (c *Coordinator) legacy(ctx context.Context, host Host, request protocol.Control) (protocol.Control, error) {
	if c.LegacyExchange != nil {
		return c.LegacyExchange(ctx, host, request)
	}
	return Exchange(ctx, host, request)
}

func (c *Coordinator) bootstrap(ctx context.Context, run Run, index int) error {
	target := run.Targets[index]
	if target.State == Failed && !target.BootstrapRetry {
		return nil
	}
	if target.BootstrapSession != "" {
		return c.bootstrapProgress(ctx, run, index)
	}
	if run.Cancel {
		return c.bootstrapReceipt(ctx, run, index)
	}
	current, err := c.reserveBootstrap(run, index)
	if err != nil {
		return err
	}
	target = current.Targets[index]
	if !target.Grant || target.BootstrapAttempt == 0 {
		return nil
	}
	return c.launchBootstrap(ctx, current, index)
}

func (c *Coordinator) reserveBootstrap(run Run, index int) (Run, error) {
	return c.Store.Change(run.ID, func(current *Run) error {
		target := &current.Targets[index]
		if target.Grant && target.BootstrapAttempt != 0 {
			return nil
		}
		if !canGrant(*current, index) {
			return nil
		}
		target.Grant, target.State = true, Granted
		if target.Generation == 0 {
			target.Generation = uint64(current.CreatedAt.UnixMilli())
		}
		if target.BootstrapAttempt == 0 {
			target.BootstrapAttempt = 1
		}
		return nil
	})
}

func bootstrapRequest(run Run, index int) updatebootstrap.Request {
	target := run.Targets[index]
	return updatebootstrap.Request{ID: run.ID, TargetID: target.Host.ID, CoordinatorID: run.Coordinator, Generation: target.Generation, Manifest: run.Release, RetryToken: target.BootstrapRetryToken}
}

func (c *Coordinator) launchBootstrap(ctx context.Context, run Run, index int) error {
	target := run.Targets[index]
	identity, err := c.legacy(ctx, target.Host, protocol.Control{Type: protocol.TypeHostInfo, RequestID: run.ID + "-identity"})
	if err != nil {
		return c.recordError(run.ID, index, err)
	}
	if identity.Host == nil || identity.Host.ID != target.Host.ID {
		return c.legacyFailure(run.ID, index, errors.New("legacy target identity changed"), true)
	}
	command, err := updatebootstrap.Command(bootstrapRequest(run, index))
	if err != nil {
		return c.legacyFailure(run.ID, index, err, true)
	}
	requestID := fmt.Sprintf("%s-bootstrap-%d", run.ID, target.BootstrapAttempt)
	response, err := c.legacy(ctx, target.Host, protocol.Control{Type: protocol.TypeCreate, RequestID: requestID, Command: command, Cols: 80, Rows: 24, Term: "dumb"})
	if err != nil {
		return c.recordError(run.ID, index, err)
	}
	if response.Type != protocol.TypeCreated || response.SessionID == "" {
		return c.legacyFailure(run.ID, index, errors.New("legacy bootstrap did not return a session receipt"), false)
	}
	return c.record(run.ID, index, func(target *Target) {
		target.BootstrapSession = response.SessionID
		target.State, target.Problem = Granted, "installing first updater"
		target.RetryAt = time.Now().Add(5 * time.Second)
	})
}

func (c *Coordinator) bootstrapProgress(ctx context.Context, run Run, index int) error {
	target := run.Targets[index]
	if target.BootstrapStatusSession != "" && !target.BootstrapRetry {
		return c.readBootstrapReceipt(ctx, run, index)
	}
	session, err := c.legacySession(ctx, run, target.Host, target.BootstrapSession)
	if err != nil {
		return c.recordError(run.ID, index, err)
	}
	if session != nil && !sessionEnded(*session) {
		return c.waitBootstrap(run.ID, index, "bootstrap command is still running")
	}
	if target.BootstrapRetry && !run.Cancel {
		return c.restartBootstrap(run, index, true)
	}
	return c.bootstrapReceipt(ctx, run, index)
}

func (c *Coordinator) legacySession(ctx context.Context, run Run, host Host, id string) (*protocol.SessionInfo, error) {
	response, err := c.legacy(ctx, host, protocol.Control{Type: protocol.TypeList, RequestID: run.ID + "-bootstrap-sessions"})
	if err != nil {
		return nil, err
	}
	if response.Type != protocol.TypeListed {
		return nil, errors.New("legacy daemon did not return its session inventory")
	}
	for _, session := range response.Sessions {
		if session.ID == id {
			return &session, nil
		}
	}
	return nil, nil
}

func sessionEnded(session protocol.SessionInfo) bool {
	return session.ExitCode != nil || session.State == "exited" || session.State == "interrupted"
}

func (c *Coordinator) bootstrapReceipt(ctx context.Context, run Run, index int) error {
	target := run.Targets[index]
	if target.BootstrapStatusSession != "" {
		return c.readBootstrapReceipt(ctx, run, index)
	}
	if !target.Grant && run.Cancel {
		return c.record(run.ID, index, func(target *Target) { target.State = Cancelled })
	}
	current, err := c.Store.Change(run.ID, func(current *Run) error {
		if current.Targets[index].BootstrapStatusAttempt == 0 {
			current.Targets[index].BootstrapStatusAttempt = 1
		}
		return nil
	})
	if err != nil {
		return err
	}
	target = current.Targets[index]
	command, err := updatebootstrap.StatusCommand(bootstrapRequest(current, index))
	if err != nil {
		return c.legacyFailure(run.ID, index, err, false)
	}
	requestID := fmt.Sprintf("%s-bootstrap-receipt-%d", run.ID, target.BootstrapStatusAttempt)
	response, err := c.legacy(ctx, target.Host, protocol.Control{Type: protocol.TypeCreate, RequestID: requestID, Command: command, Cols: 80, Rows: 24, Term: "dumb"})
	if err != nil {
		return c.recordError(run.ID, index, err)
	}
	if response.Type != protocol.TypeCreated || response.SessionID == "" {
		return c.legacyFailure(run.ID, index, errors.New("legacy status command did not return a session receipt"), false)
	}
	return c.record(run.ID, index, func(target *Target) {
		target.BootstrapStatusSession = response.SessionID
		target.RetryAt = time.Now().Add(time.Second)
	})
}

func (c *Coordinator) readBootstrapReceipt(ctx context.Context, run Run, index int) error {
	target := run.Targets[index]
	session, err := c.legacySession(ctx, run, target.Host, target.BootstrapStatusSession)
	if err != nil {
		return c.recordError(run.ID, index, err)
	}
	if session == nil || session.State == "interrupted" {
		return c.nextBootstrapReceipt(run.ID, index, "read-only bootstrap receipt session was interrupted")
	}
	if !sessionEnded(*session) {
		return c.waitBootstrap(run.ID, index, "reading durable bootstrap receipt")
	}
	response, err := c.legacy(ctx, target.Host, protocol.Control{Type: protocol.TypeLogs, RequestID: run.ID + "-bootstrap-receipt-logs", SessionID: target.BootstrapStatusSession, Tail: protocol.MaxLogTail})
	if err != nil {
		return c.recordError(run.ID, index, err)
	}
	if response.Type != protocol.TypeLogged || response.SessionID != target.BootstrapStatusSession {
		return c.legacyFailure(run.ID, index, errors.New("bootstrap receipt log identity mismatch"), false)
	}
	if session.ExitCode != nil && *session.ExitCode == 3 && bytes.Contains(response.Output, []byte("MESH_UPDATE_RECEIPT_UNAVAILABLE")) {
		return c.restartBootstrap(run, index, false)
	}
	if session.ExitCode != nil && *session.ExitCode != 0 {
		return c.legacyFailure(run.ID, index, fmt.Errorf("bootstrap receipt command exited %d; inspect session %s", *session.ExitCode, session.ID), false)
	}
	status, err := decodeBootstrapReceipt(response.Output, run, index)
	if err != nil {
		return c.legacyFailure(run.ID, index, err, false)
	}
	return c.applyBootstrapReceipt(ctx, run, index, status)
}

func decodeBootstrapReceipt(output []byte, run Run, index int) (updateinstall.Status, error) {
	var status updateinstall.Status
	if len(output) >= protocol.MaxLogTail {
		return status, errors.New("bootstrap receipt exceeds the bounded log response")
	}
	marker := []byte("MESH_UPDATE_RECEIPT=")
	position := bytes.Index(output, marker)
	if position < 0 {
		return status, errors.New("bootstrap receipt envelope is missing")
	}
	decoder := json.NewDecoder(bytes.NewReader(output[position+len(marker):]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&status); err != nil {
		return status, fmt.Errorf("decode durable bootstrap receipt: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return status, errors.New("trailing bootstrap receipt data")
	}
	target := run.Targets[index]
	if status.Schema != 1 || status.Request.ID != run.ID || status.Request.TargetID != target.Host.ID || status.Request.Generation != target.Generation || status.Request.Manifest.Digest() != run.ReleaseDigest {
		return status, errors.New("bootstrap receipt differs from the approved operation")
	}
	return status, nil
}

func (c *Coordinator) applyBootstrapReceipt(ctx context.Context, run Run, index int, status updateinstall.Status) error {
	switch status.Phase {
	case updateinstall.Committed:
		var info Info
		err := c.Remote.Call(ctx, run.Targets[index].Host, "info", nil, &info)
		if IsLegacyError(err) {
			return c.legacyFailure(run.ID, index, errors.New("committed bootstrap receipt is not backed by an executing updater daemon"), true)
		}
		if err != nil {
			return c.recordError(run.ID, index, err)
		}
		if info.Health.HostID != run.Targets[index].Host.ID {
			return c.legacyFailure(run.ID, index, errors.New("committed bootstrap target health identity mismatch"), false)
		}
		info.Installation = &status
		return c.reconcileInstallation(ctx, run, index, info)
	case updateinstall.RolledBack, updateinstall.Failed:
		return c.legacyFailure(run.ID, index, fmt.Errorf("%s: %s", status.Phase, status.Error), true)
	case updateinstall.RollbackFailed:
		return c.legacyFailure(run.ID, index, fmt.Errorf("rollback failed: %s", status.Error), false)
	case updateinstall.Cancelled:
		return c.record(run.ID, index, func(target *Target) { target.State, target.Grant, target.Problem = Cancelled, false, "" })
	case updateinstall.Accepted, updateinstall.Staged:
		return c.restartBootstrap(run, index, false)
	case updateinstall.Granted, updateinstall.Activating, updateinstall.Validating, updateinstall.RollingBack:
		return c.nextBootstrapReceipt(run.ID, index, "supervised bootstrap activation is still running")
	default:
		return c.legacyFailure(run.ID, index, errors.New("unknown bootstrap installation phase"), false)
	}
}

func (c *Coordinator) restartBootstrap(run Run, index int, explicit bool) error {
	if run.Cancel {
		return c.waitBootstrap(run.ID, index, "cancellation stopped bootstrap replay; waiting for target acknowledgement")
	}
	if !explicit && run.Targets[index].BootstrapAttempt >= 3 {
		return c.legacyFailure(run.ID, index, errors.New("bootstrap setup did not finish after three resumable attempts; explicit retry required"), true)
	}
	_, err := c.Store.Change(run.ID, func(current *Run) error {
		if current.Cancel || (!current.Targets[index].Grant && !canGrant(*current, index)) {
			return nil
		}
		target := &current.Targets[index]
		target.Grant, target.State = true, Granted
		target.BootstrapAttempt++
		if explicit {
			target.BootstrapRetryToken = target.BootstrapAttempt
			target.BootstrapRetry = false
		}
		target.BootstrapSession, target.BootstrapStatusSession = "", ""
		target.BootstrapStatusAttempt++
		target.Problem = "resuming approved bootstrap setup"
		target.RetryAt = time.Now().Add(5 * time.Second)
		return nil
	})
	return err
}

func (c *Coordinator) waitBootstrap(id string, index int, problem string) error {
	return c.record(id, index, func(target *Target) {
		target.State = Granted
		target.Problem = problem
		target.RetryAt = time.Now().Add(5 * time.Second)
	})
}

func (c *Coordinator) nextBootstrapReceipt(id string, index int, problem string) error {
	return c.record(id, index, func(target *Target) {
		target.BootstrapStatusSession = ""
		target.BootstrapStatusAttempt++
		target.State = Granted
		target.Problem = problem
		target.RetryAt = time.Now().Add(15 * time.Second)
	})
}

func (c *Coordinator) legacyFailure(id string, index int, problem error, resolved bool) error {
	_, err := c.Store.Change(id, func(run *Run) error {
		run.Stopped = true
		run.Targets[index].State, run.Targets[index].Problem = Failed, problem.Error()
		if resolved {
			run.Targets[index].Grant = false
		}
		return nil
	})
	return err
}
