package daemon

import (
	"context"
	"fmt"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"slices"
	"time"
)

// serveState owns one dedicated control reader and writes directly to the transport.
// The keyed broker queue is the only application-level event buffer.
func (s *clientServer) serveState(ctx context.Context, conn transport.Conn, request protocol.Control) error {
	if err := validateRequestID(request); err != nil {
		return s.writeStateError(ctx, conn, request, err)
	}
	if request.Watch == nil {
		return s.writeStateError(ctx, conn, request, fmt.Errorf("daemon: state.watch requires topics"))
	}
	if err := request.Watch.Validate(); err != nil {
		return s.writeStateError(ctx, conn, request, err)
	}
	if controller, ok := s.services.(*serviceController); ok && slices.Contains(request.Watch.Topics, protocol.TopicServices) {
		readCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		controller.observeRegistry(readCtx, s.state.observeServices)
		cancel()
	}
	sub, initial, err := s.state.subscribe(*request.Watch)
	if err != nil {
		return s.writeStateError(ctx, conn, request, err)
	}
	defer s.state.unsubscribe(sub)
	if sub.topics[protocol.TopicMetrics] {
		release := s.metrics.Demand(request.Watch.MetricsEvery())
		defer release()
	}
	return s.streamState(ctx, conn, request.RequestID, sub, initial)
}
func (s *clientServer) streamState(ctx context.Context, conn transport.Conn, requestID string, sub *stateSubscriber, initial *protocol.StateSnapshot) error {
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(lifetime, func() { _ = conn.Close() })
	defer stop()
	requests := make(chan protocol.Frame)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		readStateRequests(lifetime, conn, requests, cancel)
	}()
	defer func() { cancel(); <-readerDone }()
	if err := writeState(lifetime, conn, protocol.Control{Type: protocol.TypeStateSnapshot, RequestID: requestID, StateSnapshot: initial}); err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-lifetime.Done():
			return nil
		case frame := <-requests:
			if err := writeState(lifetime, conn, s.stateRequest(lifetime, frame)); err != nil {
				return err
			}
		case <-sub.wake:
			if err := writeStateBatch(lifetime, conn, s.state.take(sub)); err != nil {
				return err
			}
		case <-ticker.C:
			if err := writeState(lifetime, conn, protocol.Control{Type: protocol.TypeStateCurrent, StateCurrent: s.state.current(sub)}); err != nil {
				return err
			}
		}
	}
}
func (s *clientServer) writeStateError(ctx context.Context, conn transport.Conn, request protocol.Control, err error) error {
	return writeState(ctx, conn, protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, ErrorCode: clientErrorCode(err), Message: err.Error()})
}
func writeStateBatch(ctx context.Context, conn transport.Conn, messages []protocol.Control) error {
	for _, message := range messages {
		if err := writeState(ctx, conn, message); err != nil {
			return err
		}
	}
	return nil
}
func writeState(ctx context.Context, conn transport.Conn, message protocol.Control) error {
	frame, err := encodeClientControl(message)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(writeCtx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.WriteFrame(frame); err != nil {
		return fmt.Errorf("daemon: write %s: %w", message.Type, err)
	}
	if err := writeCtx.Err(); err != nil {
		return fmt.Errorf("daemon: write %s deadline: %w", message.Type, err)
	}
	return nil
}
