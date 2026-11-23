package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// errRunPoolFull reports that every run slot for a host is busy. Nothing was
// sent.
var errRunPoolFull = errors.New("every mesh_run slot for this host is busy")

// errInvalidRunReply reports a reply that does not answer the run that was
// sent. The run may have happened.
var errInvalidRunReply = errors.New("host sent an invalid execution.run reply")

// runProbeIdle is how long a pooled connection may sit idle before a run
// checks it is still alive.
const runProbeIdle = 10 * time.Second

// runProbeTimeout bounds that check on its own. A variable so tests can
// shorten it.
var runProbeTimeout = 2 * time.Second

// ExchangeRun sends one execution.run on a pooled connection from the host's
// run budget and waits for its single reply. It never retries: once the
// write was attempted, a failure leaves the outcome unknown. A connection
// goes back to the pool only after a valid reply, and only if the call's
// cancellation could no longer close it.
func (p *mcpHostPool) ExchangeRun(ctx context.Context, host HostRecord, req protocol.Control) (protocol.Control, protocol.HostInfo, error) {
	bucket, err := p.bucket(host)
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
	}
	select {
	case bucket.runSlots <- struct{}{}:
	default:
		return protocol.Control{}, protocol.HostInfo{}, errRunPoolFull
	}
	defer func() { <-bucket.runSlots }()
	if req.RequestID == "" {
		if req.RequestID, err = newDaemonRequestID(); err != nil {
			return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
		}
	}
	entry, err := p.runConnection(ctx, bucket, host)
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
	}
	resp, reusable, err := exchangeOneFrame(ctx, entry.conn, req)
	if err == nil {
		err = validRunReply(resp, req.OutputBytes)
	}
	if err != nil {
		p.abandon(entry.conn)
		return protocol.Control{}, entry.info, err
	}
	// A valid reply is the run's result even when the call's cancellation
	// fired as it arrived; only the connection it may be closing is dropped.
	p.settleRunConnection(bucket, entry, reusable)
	return resp, entry.info, nil
}

func (p *mcpHostPool) settleRunConnection(bucket *mcpHostBucket, entry *mcpPooledConn, reusable bool) {
	if !reusable {
		p.abandon(entry.conn)
		return
	}
	p.checkin(bucket, entry)
}

// abandon drops a run's connection without a close handshake. A graceful
// Close could claim the connection's end before the cancellation's Abort
// arrives and then wait on a peer that never answers.
func (p *mcpHostPool) abandon(conn transport.Conn) {
	p.mu.Lock()
	delete(p.live, conn)
	p.mu.Unlock()
	_ = transport.Abort(conn)
}

func validRunReply(resp protocol.Control, outputBytes int) error {
	if resp.Type == protocol.TypeError && resp.ErrorCode != "" {
		return nil
	}
	if resp.Type != protocol.TypeExecutionRunResult {
		return errInvalidRunReply
	}
	if err := resp.ExecutionRun.Validate(outputBytes); err != nil {
		return fmt.Errorf("%w: %w", errInvalidRunReply, err)
	}
	return nil
}

// runConnection is a connection a run may use. A run can never be retried,
// so it must not be the request that finds out the host closed a
// connection while it sat idle (a daemon restart, a network change): one
// idle for a while answers a host.info first, or a fresh one is dialled.
func (p *mcpHostPool) runConnection(ctx context.Context, bucket *mcpHostBucket, host HostRecord) (*mcpPooledConn, error) {
	entry, reused, err := p.checkout(ctx, bucket, host)
	if err != nil || !reused {
		return entry, err
	}
	if p.usableForRun(ctx, host, entry) {
		return entry, nil
	}
	p.abandon(entry.conn)
	fresh, _, err := p.checkoutFresh(ctx, host)
	return fresh, err
}

// usableForRun rejects a pooled connection its transport already knows is
// dead (the host revoked the device or restarted), and checks one that sat
// idle for a while with a host.info.
func (p *mcpHostPool) usableForRun(ctx context.Context, host HostRecord, entry *mcpPooledConn) bool {
	if transport.Stopped(entry.conn) {
		return false
	}
	if p.now().Sub(entry.idleAt) < runProbeIdle {
		return true
	}
	info, ok := probeRunConnection(ctx, host, entry.conn)
	if ok {
		entry.info = info
	}
	return ok
}

// probeRunConnection reports whether conn answered a host.info that
// validates like a fresh connection's, on its own short deadline, with its
// cancellation never having fired.
func probeRunConnection(ctx context.Context, host HostRecord, conn transport.Conn) (protocol.HostInfo, bool) {
	requestID, err := newDaemonRequestID()
	if err != nil {
		return protocol.HostInfo{}, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, runProbeTimeout)
	defer cancel()
	resp, reusable, err := exchangeOneFrame(probeCtx, conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID})
	if err != nil || !reusable || validateAgentHostInfo(host, resp) != nil {
		return protocol.HostInfo{}, false
	}
	return *resp.Host, true
}

// exchangeOneFrame writes one control and reads exactly one control frame
// back. A context already done is refused before the write, as nothing was
// sent; after the write is attempted every failure is the caller's to
// classify. reusable is false when the cancellation callback ran or is
// running, since it closes the connection.
func exchangeOneFrame(ctx context.Context, conn transport.Conn, req protocol.Control) (protocol.Control, bool, error) {
	payload, err := req.Encode()
	if err != nil {
		return protocol.Control{}, false, &preDispatchError{err: fmt.Errorf("encode %s: %w", req.Type, err)}
	}
	if err := ctx.Err(); err != nil {
		return protocol.Control{}, false, &preDispatchError{err: err}
	}
	// Abort, not Close: a graceful close waits for a peer that may never
	// answer, and the caller has already given up on this connection.
	stop := context.AfterFunc(ctx, func() { _ = transport.Abort(conn) })
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		stop()
		return protocol.Control{}, false, daemonOperationError(ctx, "write "+req.Type+" request", err)
	}
	frame, err := conn.ReadFrame()
	stopped := stop()
	if err != nil {
		return protocol.Control{}, false, daemonOperationError(ctx, "read "+req.Type+" reply", err)
	}
	resp, err := decodeOneReply(frame, req.RequestID)
	return resp, stopped && err == nil, err
}

func decodeOneReply(frame protocol.Frame, requestID string) (protocol.Control, error) {
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, errInvalidRunReply
	}
	resp, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("%w: %w", errInvalidRunReply, err)
	}
	if resp.RequestID != requestID {
		return protocol.Control{}, errInvalidRunReply
	}
	return resp, nil
}
