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

// ExchangeRun sends one execution.run on a pooled connection from the host's
// run budget and waits for its single reply. It never retries: once the
// frame may have left, a failure leaves the outcome unknown. A connection
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
	payload, err := req.Encode()
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: fmt.Errorf("encode execution.run: %w", err)}
	}
	entry, err := p.runConnection(ctx, bucket, host)
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
	}
	resp, reusable, err := exchangeRunFrame(ctx, entry.conn, req.RequestID, payload)
	if err != nil || !reusable {
		p.discard(entry.conn)
		return resp, entry.info, err
	}
	p.checkin(bucket, entry)
	return resp, entry.info, nil
}

// runProbeIdle is how long a pooled connection may sit idle before a run
// checks it is still alive.
const runProbeIdle = 10 * time.Second

// runConnection is a connection a run may use. A run can never be retried,
// so it must not be the request that finds out the host closed a
// connection while it sat idle (a daemon restart, a network change): one
// idle for a while answers a host.info first, or a fresh one is dialled.
func (p *mcpHostPool) runConnection(ctx context.Context, bucket *mcpHostBucket, host HostRecord) (*mcpPooledConn, error) {
	entry, reused, err := p.checkout(ctx, bucket, host)
	if err != nil || !reused || p.now().Sub(entry.idleAt) < runProbeIdle {
		return entry, err
	}
	if probeRunConnection(ctx, entry.conn) == nil {
		return entry, nil
	}
	p.discard(entry.conn)
	fresh, _, err := p.checkoutFresh(ctx, host)
	return fresh, err
}

func probeRunConnection(ctx context.Context, conn transport.Conn) error {
	requestID, err := newDaemonRequestID()
	if err != nil {
		return err
	}
	resp, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID})
	if err == nil && resp.Type != protocol.TypeHostInfoResult {
		err = fmt.Errorf("probe pooled connection: unexpected %s reply", resp.Type)
	}
	return err
}

// exchangeRunFrame writes one run and reads its reply. reusable is false when
// the cancellation callback already ran or is running, since it closes the
// connection.
func exchangeRunFrame(ctx context.Context, conn transport.Conn, requestID string, payload []byte) (protocol.Control, bool, error) {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		stop()
		return protocol.Control{}, false, daemonOperationError(ctx, "write execution.run request", err)
	}
	resp, err := protocol.ReadControl(conn.ReadFrame, conn.WriteFrame)
	stopped := stop()
	if err != nil {
		return protocol.Control{}, false, daemonOperationError(ctx, "read execution.run reply", err)
	}
	if !validRunReply(resp, requestID) {
		return protocol.Control{}, false, errInvalidRunReply
	}
	return resp, stopped, nil
}

func validRunReply(resp protocol.Control, requestID string) bool {
	if resp.RequestID != requestID {
		return false
	}
	if resp.Type == protocol.TypeError {
		return true
	}
	return resp.Type == protocol.TypeExecutionRunResult && resp.ExecutionRun != nil
}
