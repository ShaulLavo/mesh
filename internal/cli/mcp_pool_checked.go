package cli

import (
	"context"
	"errors"

	"github.com/shaul/mesh/internal/protocol"
)

// ExchangeShortChecked reads a fresh host observation on the checked-out
// connection, lets check refuse it, and only then sends req on that same
// connection. A pooled connection's observation is from when it was dialed,
// and a mutation must not be sent before a precondition on the host's
// current state is known to hold. Mutations are never retried.
func (p *mcpHostPool) ExchangeShortChecked(ctx context.Context, host HostRecord, req protocol.Control, check func(protocol.HostInfo) error) (protocol.Control, protocol.HostInfo, error) {
	bucket, err := p.bucket(host)
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
	}
	if err := acquireSlot(ctx, bucket.shortSlots); err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
	}
	defer func() { <-bucket.shortSlots }()
	entry, _, err := p.checkout(ctx, bucket, host)
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
	}
	info, err := p.observeOn(ctx, entry)
	if err == nil {
		err = check(info)
	}
	if err != nil {
		p.checkin(bucket, entry)
		return protocol.Control{}, info, &preDispatchError{err: err}
	}
	if req.RequestID == "" {
		if req.RequestID, err = newDaemonRequestID(); err != nil {
			p.checkin(bucket, entry)
			return protocol.Control{}, info, &preDispatchError{err: err}
		}
	}
	resp, err := controlRequest(ctx, entry.conn, req)
	if err != nil {
		p.discard(entry.conn)
		return protocol.Control{}, info, err
	}
	p.checkin(bucket, entry)
	return resp, info, nil
}

func (p *mcpHostPool) observeOn(ctx context.Context, entry *mcpPooledConn) (protocol.HostInfo, error) {
	id, err := newDaemonRequestID()
	if err != nil {
		return protocol.HostInfo{}, err
	}
	resp, err := controlRequest(ctx, entry.conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: id})
	if err != nil {
		return protocol.HostInfo{}, err
	}
	if resp.Type != protocol.TypeHostInfoResult || resp.Host == nil {
		return protocol.HostInfo{}, errors.New("host returned no host observation")
	}
	return *resp.Host, nil
}
